#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <shlobj.h>
#include <olectl.h>
#include <stdint.h>
#include <wchar.h>

#define IDI_OVERLAY_PINNED  101
#define IDI_OVERLAY_CACHED  102
#define IDI_OVERLAY_CLOUD   103
#define IDI_OVERLAY_SYNCING 104

// Must match internal/desktop/overlay_windows.go byte-for-byte.
static const uint32_t kSharedMagic   = 0x31534D54u; // "TMS1"
static const uint32_t kSharedVersion = 1u;

#pragma pack(push, 1)
struct TeleMountSharedState {
    uint32_t magic;
    uint32_t version;
    volatile LONG generation;
    uint16_t driveLetter; // uppercase 'A'..'Z', or 0 when unmounted
    uint16_t reserved;
};
#pragma pack(pop)

struct OverlaySpec {
    CLSID clsid;
    const wchar_t* clsidStr;
    const wchar_t* regName;
    const wchar_t* description;
    int iconResId;
    char stateChar;
};

// {4A7E2B19-6C3D-4F81-B905-2D8A7E1C4F41..44}
static const OverlaySpec kOverlaySpecs[4] = {
    {
        {0x4A7E2B19, 0x6C3D, 0x4F81, {0xB9, 0x05, 0x2D, 0x8A, 0x7E, 0x1C, 0x4F, 0x41}},
        L"{4A7E2B19-6C3D-4F81-B905-2D8A7E1C4F41}",
        L"  TeleMount1Pinned",
        L"TeleMount Always Offline Overlay",
        IDI_OVERLAY_PINNED,
        'P',
    },
    {
        {0x4A7E2B19, 0x6C3D, 0x4F81, {0xB9, 0x05, 0x2D, 0x8A, 0x7E, 0x1C, 0x4F, 0x42}},
        L"{4A7E2B19-6C3D-4F81-B905-2D8A7E1C4F42}",
        L"  TeleMount2Cached",
        L"TeleMount Available Offline Overlay",
        IDI_OVERLAY_CACHED,
        'C',
    },
    {
        {0x4A7E2B19, 0x6C3D, 0x4F81, {0xB9, 0x05, 0x2D, 0x8A, 0x7E, 0x1C, 0x4F, 0x43}},
        L"{4A7E2B19-6C3D-4F81-B905-2D8A7E1C4F43}",
        L"  TeleMount3Cloud",
        L"TeleMount Cloud-Only Overlay",
        IDI_OVERLAY_CLOUD,
        'O',
    },
    {
        {0x4A7E2B19, 0x6C3D, 0x4F81, {0xB9, 0x05, 0x2D, 0x8A, 0x7E, 0x1C, 0x4F, 0x44}},
        L"{4A7E2B19-6C3D-4F81-B905-2D8A7E1C4F44}",
        L"  TeleMount4Syncing",
        L"TeleMount Syncing Overlay",
        IDI_OVERLAY_SYNCING,
        'S',
    },
};

static HMODULE g_hModule = nullptr;
static volatile LONG g_dllRefCount = 0;

static SRWLOCK g_stateLock = SRWLOCK_INIT;
static HANDLE g_shmHandle = nullptr;
static const TeleMountSharedState* g_shmView = nullptr;
static ULONGLONG g_lastShmTryMs = 0;
static ULONGLONG g_pipeBackoffUntilMs = 0;
static LONG g_pipeBackoffGen = -1;
static wchar_t g_pipeName[96] = {};

static const size_t kCacheSlots = 256;
static const size_t kCacheMaxPath = 260;

struct CacheEntry {
    bool valid;
    char state;
    LONG generation;
    ULONGLONG tickMs;
    size_t pathLen;
    wchar_t path[kCacheMaxPath];
};

static CacheEntry g_cache[kCacheSlots] = {};

static void initPipeName() {
    DWORD sid = 0;
    ProcessIdToSessionId(GetCurrentProcessId(), &sid);
    wsprintfW(g_pipeName, L"\\\\.\\pipe\\TeleMountOverlay_%lu", sid);
}

static inline wchar_t asciiUpper(wchar_t c) {
    return (c >= L'a' && c <= L'z') ? static_cast<wchar_t>(c - (L'a' - L'A')) : c;
}

static inline wchar_t asciiLower(wchar_t c) {
    return (c >= L'A' && c <= L'Z') ? static_cast<wchar_t>(c + (L'a' - L'A')) : c;
}

static uint64_t hashPathCaseInsensitive(const wchar_t* s, size_t len) {
    uint64_t h = 14695981039346656037ULL;
    for (size_t i = 0; i < len; ++i) {
        h ^= static_cast<uint64_t>(asciiLower(s[i]));
        h *= 1099511628211ULL;
    }
    return h;
}

static bool readSharedHeader(wchar_t* outDrive, LONG* outGen) {
    ULONGLONG now = GetTickCount64();

    AcquireSRWLockShared(&g_stateLock);
    const TeleMountSharedState* view = g_shmView;
    if (view != nullptr) {
        uint32_t magic = view->magic;
        uint32_t ver = view->version;
        LONG gen = view->generation;
        wchar_t drv = static_cast<wchar_t>(view->driveLetter);
        MemoryBarrier();
        ReleaseSRWLockShared(&g_stateLock);
        if (magic != kSharedMagic || ver != kSharedVersion || drv < L'A' || drv > L'Z') {
            return false;
        }
        *outDrive = drv;
        *outGen = gen;
        return true;
    }
    ULONGLONG lastTry = g_lastShmTryMs;
    ReleaseSRWLockShared(&g_stateLock);

    // Avoid spamming OpenFileMappingW when TeleMount is not running.
    if (lastTry != 0 && (now - lastTry) < 1000) {
        return false;
    }

    AcquireSRWLockExclusive(&g_stateLock);
    if (g_shmView == nullptr) {
        g_lastShmTryMs = now;
        HANDLE h = OpenFileMappingW(FILE_MAP_READ, FALSE, L"Local\\TeleMountShellState");
        if (h != nullptr) {
            void* p = MapViewOfFile(h, FILE_MAP_READ, 0, 0, sizeof(TeleMountSharedState));
            if (p != nullptr) {
                g_shmHandle = h;
                g_shmView = static_cast<const TeleMountSharedState*>(p);
            } else {
                CloseHandle(h);
            }
        }
    }
    view = g_shmView;
    bool ok = false;
    if (view != nullptr) {
        uint32_t magic = view->magic;
        uint32_t ver = view->version;
        LONG gen = view->generation;
        wchar_t drv = static_cast<wchar_t>(view->driveLetter);
        MemoryBarrier();
        if (magic == kSharedMagic && ver == kSharedVersion && drv >= L'A' && drv <= L'Z') {
            *outDrive = drv;
            *outGen = gen;
            ok = true;
        }
    }
    ReleaseSRWLockExclusive(&g_stateLock);
    return ok;
}

static char queryPathState(LPCWSTR rawPath) {
    if (!rawPath || !rawPath[0]) {
        return 'N';
    }

    wchar_t activeDrive = 0;
    LONG curGen = 0;
    if (!readSharedHeader(&activeDrive, &curGen)) {
        return 'N';
    }

    const wchar_t* p = rawPath;
    if (p[0] == L'\\' && p[1] == L'\\' && p[2] == L'?' && p[3] == L'\\') {
        p += 4;
    }
    // Ultra-fast reject for all drives other than TeleMount's active drive letter.
    if (asciiUpper(p[0]) != activeDrive || p[1] != L':' || p[2] != L'\\') {
        return 'N';
    }
    // Keep drive root ("V:\") clean without an overlay badge.
    if (p[3] == L'\0') {
        return 'N';
    }

    const wchar_t* relW = p + 2; // starts with '\'
    size_t relLen = wcslen(relW);
    ULONGLONG now = GetTickCount64();
    size_t slot = static_cast<size_t>(hashPathCaseInsensitive(relW, relLen) & (kCacheSlots - 1));

    AcquireSRWLockShared(&g_stateLock);
    if (relLen < kCacheMaxPath) {
        const CacheEntry& ce = g_cache[slot];
        if (ce.valid && ce.generation == curGen && (now - ce.tickMs) < 3000 &&
            ce.pathLen == relLen && _wcsicmp(ce.path, relW) == 0) {
            char hit = ce.state;
            ReleaseSRWLockShared(&g_stateLock);
            return hit;
        }
    }
    bool backedOff = (now < g_pipeBackoffUntilMs && curGen == g_pipeBackoffGen);
    ReleaseSRWLockShared(&g_stateLock);
    if (backedOff) {
        return 'N';
    }

    // Encode virtual POSIX path in UTF-8 ("/Folder/file.ext") on the stack for typical paths.
    char utf8Buf[4096];
    int utf8Len = WideCharToMultiByte(CP_UTF8, 0, relW, static_cast<int>(relLen), utf8Buf, sizeof(utf8Buf) - 1, nullptr, nullptr);
    if (utf8Len <= 0 || utf8Len >= static_cast<int>(sizeof(utf8Buf))) {
        return 'N';
    }
    for (int i = 0; i < utf8Len; ++i) {
        if (utf8Buf[i] == '\\') {
            utf8Buf[i] = '/';
        }
    }

    char resp = 'N';
    DWORD bytesRead = 0;
    BOOL ok = CallNamedPipeW(
        g_pipeName,
        utf8Buf,
        static_cast<DWORD>(utf8Len),
        &resp,
        1,
        &bytesRead,
        40
    );
    if (!ok || bytesRead == 0) {
        DWORD err = GetLastError();
        if (err == ERROR_FILE_NOT_FOUND || err == ERROR_PIPE_NOT_CONNECTED || err == ERROR_BROKEN_PIPE) {
            AcquireSRWLockExclusive(&g_stateLock);
            g_pipeBackoffUntilMs = GetTickCount64() + 1500;
            g_pipeBackoffGen = curGen;
            ReleaseSRWLockExclusive(&g_stateLock);
        }
        return 'N';
    }

    if (relLen < kCacheMaxPath) {
        AcquireSRWLockExclusive(&g_stateLock);
        CacheEntry& ce = g_cache[slot];
        ce.valid = true;
        ce.state = resp;
        ce.generation = curGen;
        ce.tickMs = GetTickCount64();
        ce.pathLen = relLen;
        memcpy(ce.path, relW, (relLen + 1) * sizeof(wchar_t));
        ReleaseSRWLockExclusive(&g_stateLock);
    }
    return resp;
}

class TeleMountOverlay final : public IShellIconOverlayIdentifier {
public:
    explicit TeleMountOverlay(const OverlaySpec& spec) : m_ref(1), m_spec(spec) {
        InterlockedIncrement(&g_dllRefCount);
    }

    ~TeleMountOverlay() {
        InterlockedDecrement(&g_dllRefCount);
    }

    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IShellIconOverlayIdentifier) {
            *ppv = static_cast<IShellIconOverlayIdentifier*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }

    STDMETHODIMP_(ULONG) AddRef() override {
        return static_cast<ULONG>(InterlockedIncrement(&m_ref));
    }

    STDMETHODIMP_(ULONG) Release() override {
        LONG count = InterlockedDecrement(&m_ref);
        if (count == 0) {
            delete this;
        }
        return static_cast<ULONG>(count);
    }

    STDMETHODIMP IsMemberOf(LPCWSTR pwszPath, DWORD /*dwAttrib*/) override {
        return (queryPathState(pwszPath) == m_spec.stateChar) ? S_OK : S_FALSE;
    }

    STDMETHODIMP GetOverlayInfo(LPWSTR pwszIconFile, int cchMax, int* pIndex, DWORD* pdwFlags) override {
        if (!pwszIconFile || !pIndex || !pdwFlags || cchMax <= 0) {
            return E_INVALIDARG;
        }
        DWORD n = GetModuleFileNameW(g_hModule, pwszIconFile, static_cast<DWORD>(cchMax));
        if (n == 0 || n >= static_cast<DWORD>(cchMax)) {
            return E_FAIL;
        }
        *pIndex = -m_spec.iconResId;
        *pdwFlags = ISIOI_ICONFILE | ISIOI_ICONINDEX;
        return S_OK;
    }

    STDMETHODIMP GetPriority(int* pPriority) override {
        if (!pPriority) return E_POINTER;
        *pPriority = 0;
        return S_OK;
    }

private:
    volatile LONG m_ref;
    const OverlaySpec& m_spec;
};

class TeleMountClassFactory final : public IClassFactory {
public:
    explicit TeleMountClassFactory(const OverlaySpec& spec) : m_ref(1), m_spec(spec) {
        InterlockedIncrement(&g_dllRefCount);
    }

    ~TeleMountClassFactory() {
        InterlockedDecrement(&g_dllRefCount);
    }

    STDMETHODIMP QueryInterface(REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        if (riid == IID_IUnknown || riid == IID_IClassFactory) {
            *ppv = static_cast<IClassFactory*>(this);
            AddRef();
            return S_OK;
        }
        *ppv = nullptr;
        return E_NOINTERFACE;
    }

    STDMETHODIMP_(ULONG) AddRef() override {
        return static_cast<ULONG>(InterlockedIncrement(&m_ref));
    }

    STDMETHODIMP_(ULONG) Release() override {
        LONG count = InterlockedDecrement(&m_ref);
        if (count == 0) {
            delete this;
        }
        return static_cast<ULONG>(count);
    }

    STDMETHODIMP CreateInstance(IUnknown* pUnkOuter, REFIID riid, void** ppv) override {
        if (!ppv) return E_POINTER;
        *ppv = nullptr;
        if (pUnkOuter != nullptr) return CLASS_E_NOAGGREGATION;
        auto* obj = new TeleMountOverlay(m_spec);
        HRESULT hr = obj->QueryInterface(riid, ppv);
        obj->Release();
        return hr;
    }

    STDMETHODIMP LockServer(BOOL fLock) override {
        if (fLock) {
            InterlockedIncrement(&g_dllRefCount);
        } else {
            InterlockedDecrement(&g_dllRefCount);
        }
        return S_OK;
    }

private:
    volatile LONG m_ref;
    const OverlaySpec& m_spec;
};

static HRESULT setRegString(HKEY root, const wchar_t* subkey, const wchar_t* name, const wchar_t* val) {
    HKEY hKey = nullptr;
    LONG rc = RegCreateKeyExW(root, subkey, 0, nullptr, REG_OPTION_NON_VOLATILE, KEY_SET_VALUE | KEY_WOW64_64KEY, nullptr, &hKey, nullptr);
    if (rc != ERROR_SUCCESS) return HRESULT_FROM_WIN32(rc);
    DWORD bytes = static_cast<DWORD>((wcslen(val) + 1) * sizeof(wchar_t));
    rc = RegSetValueExW(hKey, name, 0, REG_SZ, reinterpret_cast<const BYTE*>(val), bytes);
    RegCloseKey(hKey);
    return HRESULT_FROM_WIN32(rc);
}

BOOL APIENTRY DllMain(HMODULE hModule, DWORD reason, LPVOID reserved) {
    if (reason == DLL_PROCESS_ATTACH) {
        g_hModule = hModule;
        DisableThreadLibraryCalls(hModule);
        initPipeName();
    } else if (reason == DLL_PROCESS_DETACH && reserved == nullptr) {
        AcquireSRWLockExclusive(&g_stateLock);
        if (g_shmView != nullptr) {
            UnmapViewOfFile(g_shmView);
            g_shmView = nullptr;
        }
        if (g_shmHandle != nullptr) {
            CloseHandle(g_shmHandle);
            g_shmHandle = nullptr;
        }
        ReleaseSRWLockExclusive(&g_stateLock);
    }
    return TRUE;
}

STDAPI DllCanUnloadNow(void) {
    return (g_dllRefCount == 0) ? S_OK : S_FALSE;
}

STDAPI DllGetClassObject(REFCLSID rclsid, REFIID riid, LPVOID* ppv) {
    if (!ppv) return E_POINTER;
    *ppv = nullptr;
    for (const auto& spec : kOverlaySpecs) {
        if (IsEqualCLSID(rclsid, spec.clsid)) {
            auto* factory = new TeleMountClassFactory(spec);
            HRESULT hr = factory->QueryInterface(riid, ppv);
            factory->Release();
            return hr;
        }
    }
    return CLASS_E_CLASSNOTAVAILABLE;
}

STDAPI DllRegisterServer(void) {
    wchar_t dllPath[MAX_PATH] = {};
    DWORD n = GetModuleFileNameW(g_hModule, dllPath, MAX_PATH);
    if (n == 0 || n >= MAX_PATH) {
        return E_FAIL;
    }

    for (const auto& spec : kOverlaySpecs) {
        wchar_t keyBuf[256];
        wsprintfW(keyBuf, L"SOFTWARE\\Classes\\CLSID\\%s", spec.clsidStr);
        HRESULT hr = setRegString(HKEY_LOCAL_MACHINE, keyBuf, nullptr, spec.description);
        if (FAILED(hr)) return hr;

        wsprintfW(keyBuf, L"SOFTWARE\\Classes\\CLSID\\%s\\InprocServer32", spec.clsidStr);
        hr = setRegString(HKEY_LOCAL_MACHINE, keyBuf, nullptr, dllPath);
        if (FAILED(hr)) return hr;
        hr = setRegString(HKEY_LOCAL_MACHINE, keyBuf, L"ThreadingModel", L"Apartment");
        if (FAILED(hr)) return hr;

        wsprintfW(keyBuf, L"SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Explorer\\ShellIconOverlayIdentifiers\\%s", spec.regName);
        hr = setRegString(HKEY_LOCAL_MACHINE, keyBuf, nullptr, spec.clsidStr);
        if (FAILED(hr)) return hr;

        hr = setRegString(
            HKEY_LOCAL_MACHINE,
            L"SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Shell Extensions\\Approved",
            spec.clsidStr,
            spec.description
        );
        if (FAILED(hr)) return hr;
    }

    SHChangeNotify(SHCNE_ASSOCCHANGED, SHCNF_IDLIST, nullptr, nullptr);
    return S_OK;
}

STDAPI DllUnregisterServer(void) {
    HKEY approvedKey = nullptr;
    if (RegOpenKeyExW(
            HKEY_LOCAL_MACHINE,
            L"SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Shell Extensions\\Approved",
            0,
            KEY_SET_VALUE | KEY_WOW64_64KEY,
            &approvedKey) == ERROR_SUCCESS) {
        for (const auto& spec : kOverlaySpecs) {
            RegDeleteValueW(approvedKey, spec.clsidStr);
        }
        RegCloseKey(approvedKey);
    }

    for (const auto& spec : kOverlaySpecs) {
        wchar_t keyBuf[256];
        wsprintfW(keyBuf, L"SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Explorer\\ShellIconOverlayIdentifiers\\%s", spec.regName);
        RegDeleteTreeW(HKEY_LOCAL_MACHINE, keyBuf);

        wsprintfW(keyBuf, L"SOFTWARE\\Classes\\CLSID\\%s", spec.clsidStr);
        RegDeleteTreeW(HKEY_LOCAL_MACHINE, keyBuf);
    }

    SHChangeNotify(SHCNE_ASSOCCHANGED, SHCNF_IDLIST, nullptr, nullptr);
    return S_OK;
}
