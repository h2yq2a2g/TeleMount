package crypto

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/hkdf"
)

const (
	// Argon2id parameters for deriving a 32-byte AES-256 master key from any user passphrase.
	ArgonTime    uint32 = 3
	ArgonMemory  uint32 = 64 * 1024 // 64 MiB
	ArgonThreads uint8  = 4
	KeySize             = 32
	SaltSize            = 16

	// Chunked AEAD parameters: 64 KiB plaintext blocks give O(1) random-access seek with ~0.024% overhead.
	PlainBlockSize  = 64 * 1024
	GCMTagSize      = 16
	CipherBlockSize = PlainBlockSize + GCMTagSize
	NonceSize       = 12
	PartHeaderSize  = 4 + SaltSize + NonceSize // 32 bytes

	dbBackupHeaderSize = 4 + 1 + 4 + 4 + 1 + SaltSize + NonceSize // 42 bytes
)

var (
	PartHeaderMagic = [4]byte{'T', 'M', 'P', '1'}
	DBBackupMagic   = [4]byte{'T', 'M', 'D', 'B'}

	ErrInvalidHeader  = errors.New("invalid encrypted header")
	ErrDecryptFailed  = errors.New("decryption failed: wrong key or corrupted data")
	ErrInvalidKeySize = errors.New("master key must be 32 bytes")
)

// DeriveMasterKey derives a 32-byte AES-256 key from an arbitrary passphrase and 16-byte salt using Argon2id.
func DeriveMasterKey(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, ArgonTime, ArgonMemory, ArgonThreads, KeySize)
}

// NewSalt returns 16 cryptographically random bytes.
func NewSalt() ([]byte, error) {
	b := make([]byte, SaltSize)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return nil, err
	}
	return b, nil
}

// GenerateRandomPassphrase returns a human-copyable random passphrase (~120 bits of entropy).
func GenerateRandomPassphrase() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I ambiguity
	raw := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	var b strings.Builder
	for i, v := range raw {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(alphabet[int(v)%len(alphabet)])
	}
	return b.String(), nil
}

// VerifyKeyEqual compares two keys in constant time.
func VerifyKeyEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// CipherSize returns the exact encrypted blob length (including 32-byte header) for a plaintext part size.
func CipherSize(plainSize int64) int64 {
	if plainSize <= 0 {
		return PartHeaderSize
	}
	blocks := (plainSize + PlainBlockSize - 1) / PlainBlockSize
	return PartHeaderSize + plainSize + blocks*GCMTagSize
}

// BlockCipherBounds returns the ciphertext byte offset and length of plaintext block blockIdx inside a part.
func BlockCipherBounds(partPlainSize int64, blockIdx uint64) (offset int64, length int64, err error) {
	plainStart := int64(blockIdx) * PlainBlockSize
	if plainStart >= partPlainSize {
		return 0, 0, io.EOF
	}
	plainLen := partPlainSize - plainStart
	if plainLen > PlainBlockSize {
		plainLen = PlainBlockSize
	}
	offset = PartHeaderSize + int64(blockIdx)*CipherBlockSize
	length = plainLen + GCMTagSize
	return offset, length, nil
}

func derivePartKey(masterKey, partSalt []byte) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, ErrInvalidKeySize
	}
	h := hkdf.New(sha256.New, masterKey, partSalt, []byte("telemount-part-v1"))
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(h, key); err != nil {
		return nil, err
	}
	return key, nil
}

func blockNonce(baseNonce []byte, blockIdx uint64, dst []byte) {
	copy(dst, baseNonce)
	var ctr [8]byte
	binary.BigEndian.PutUint64(ctr[:], blockIdx)
	for i := 0; i < 8; i++ {
		dst[4+i] ^= ctr[i]
	}
}

func blockAAD(blockIdx uint64, dst []byte) {
	copy(dst[:4], PartHeaderMagic[:])
	binary.BigEndian.PutUint64(dst[4:12], blockIdx)
}

// EncryptReader streams plaintext from an underlying io.Reader into chunked AES-256-GCM ciphertext
// using a small fixed RAM footprint (~130 KiB).
type EncryptReader struct {
	src       io.Reader
	aead      cipher.AEAD
	baseNonce [NonceSize]byte
	header    [PartHeaderSize]byte

	plainBuf  []byte
	cipherBuf []byte
	pending   []byte
	blockIdx  uint64
	eof       bool
}

// NewEncryptReader wraps src to produce a TMP1 encrypted stream and returns its 32-byte header.
func NewEncryptReader(src io.Reader, masterKey []byte) (*EncryptReader, [PartHeaderSize]byte, error) {
	var hdr [PartHeaderSize]byte
	if len(masterKey) != KeySize {
		return nil, hdr, ErrInvalidKeySize
	}
	copy(hdr[0:4], PartHeaderMagic[:])
	if _, err := io.ReadFull(rand.Reader, hdr[4:20]); err != nil {
		return nil, hdr, err
	}
	if _, err := io.ReadFull(rand.Reader, hdr[20:32]); err != nil {
		return nil, hdr, err
	}

	partKey, err := derivePartKey(masterKey, hdr[4:20])
	if err != nil {
		return nil, hdr, err
	}
	blk, err := aes.NewCipher(partKey)
	if err != nil {
		return nil, hdr, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, hdr, err
	}

	er := &EncryptReader{
		src:       src,
		aead:      aead,
		header:    hdr,
		plainBuf:  make([]byte, PlainBlockSize),
		cipherBuf: make([]byte, CipherBlockSize),
	}
	copy(er.baseNonce[:], hdr[20:32])
	er.pending = er.header[:]
	return er, hdr, nil
}

func (e *EncryptReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(e.pending) > 0 {
		n := copy(p, e.pending)
		e.pending = e.pending[n:]
		return n, nil
	}
	if e.eof {
		return 0, io.EOF
	}

	// Fill up to one full 64 KiB plaintext block (short reads before EOF are coalesced so block boundaries stay exact).
	filled := 0
	for filled < PlainBlockSize {
		n, err := e.src.Read(e.plainBuf[filled:])
		filled += n
		if err == io.EOF {
			e.eof = true
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if filled == 0 {
		return 0, io.EOF
	}

	var nonce [NonceSize]byte
	var aad [12]byte
	blockNonce(e.baseNonce[:], e.blockIdx, nonce[:])
	blockAAD(e.blockIdx, aad[:])
	e.blockIdx++

	e.cipherBuf = e.aead.Seal(e.cipherBuf[:0], nonce[:], e.plainBuf[:filled], aad[:])
	n := copy(p, e.cipherBuf)
	e.pending = e.cipherBuf[n:]
	return n, nil
}

// PartDecryptor decrypts individual 64 KiB blocks of a single encrypted file part in O(1).
type PartDecryptor struct {
	aead      cipher.AEAD
	baseNonce [NonceSize]byte
}

// NewPartDecryptor initializes a block decryptor from the master key and the 32-byte TMP1 header.
func NewPartDecryptor(masterKey []byte, header []byte) (*PartDecryptor, error) {
	if len(header) != PartHeaderSize || !bytes.Equal(header[0:4], PartHeaderMagic[:]) {
		return nil, ErrInvalidHeader
	}
	partKey, err := derivePartKey(masterKey, header[4:20])
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(partKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	pd := &PartDecryptor{aead: aead}
	copy(pd.baseNonce[:], header[20:32])
	return pd, nil
}

// DecryptBlock authenticates and decrypts block blockIdx into dst (which may be nil).
func (d *PartDecryptor) DecryptBlock(blockIdx uint64, ciphertext []byte, dst []byte) ([]byte, error) {
	var nonce [NonceSize]byte
	var aad [12]byte
	blockNonce(d.baseNonce[:], blockIdx, nonce[:])
	blockAAD(blockIdx, aad[:])
	plain, err := d.aead.Open(dst[:0], nonce[:], ciphertext, aad[:])
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return plain, nil
}

// EncryptDBBackup gzips and encrypts a SQLite snapshot with a self-describing TMDB header carrying Argon2id parameters and kdfSalt.
func EncryptDBBackup(sqliteBytes, masterKey, kdfSalt []byte) ([]byte, error) {
	if len(masterKey) != KeySize {
		return nil, ErrInvalidKeySize
	}
	if len(kdfSalt) != SaltSize {
		return nil, fmt.Errorf("kdf salt must be %d bytes", SaltSize)
	}

	var gzBuf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&gzBuf, gzip.DefaultCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gz.Write(sqliteBytes); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}

	var hdr [dbBackupHeaderSize]byte
	copy(hdr[0:4], DBBackupMagic[:])
	hdr[4] = 1
	binary.BigEndian.PutUint32(hdr[5:9], ArgonTime)
	binary.BigEndian.PutUint32(hdr[9:13], ArgonMemory)
	hdr[13] = ArgonThreads
	copy(hdr[14:30], kdfSalt)
	if _, err := io.ReadFull(rand.Reader, hdr[30:42]); err != nil {
		return nil, err
	}

	blk, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}

	out := make([]byte, dbBackupHeaderSize, dbBackupHeaderSize+gzBuf.Len()+GCMTagSize)
	copy(out, hdr[:])
	out = aead.Seal(out, hdr[30:42], gzBuf.Bytes(), hdr[:])
	return out, nil
}

// DecryptDBBackup derives masterKey from passphrase using the Argon2id parameters embedded in the TMDB header,
// authenticates & decrypts the payload, and returns the raw SQLite bytes along with masterKey and kdfSalt.
func DecryptDBBackup(blob []byte, passphrase string) (sqliteBytes, masterKey, kdfSalt []byte, err error) {
	if len(blob) < dbBackupHeaderSize+GCMTagSize || !bytes.Equal(blob[0:4], DBBackupMagic[:]) || blob[4] != 1 {
		return nil, nil, nil, ErrInvalidHeader
	}
	hdr := blob[:dbBackupHeaderSize]
	t := binary.BigEndian.Uint32(hdr[5:9])
	mem := binary.BigEndian.Uint32(hdr[9:13])
	threads := hdr[13]
	if t == 0 || t > 20 || mem < 1024 || mem > 1024*1024 || threads == 0 || threads > 32 {
		return nil, nil, nil, ErrInvalidHeader
	}
	kdfSalt = append([]byte(nil), hdr[14:30]...)
	nonce := hdr[30:42]
	masterKey = argon2.IDKey([]byte(passphrase), kdfSalt, t, mem, threads, KeySize)

	blk, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, nil, nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, nil, nil, err
	}
	gzData, err := aead.Open(nil, nonce, blob[dbBackupHeaderSize:], hdr)
	if err != nil {
		return nil, nil, nil, ErrDecryptFailed
	}

	gr, err := gzip.NewReader(bytes.NewReader(gzData))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("corrupted backup gzip: %w", err)
	}
	defer gr.Close()
	sqliteBytes, err = io.ReadAll(gr)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read backup gzip: %w", err)
	}
	return sqliteBytes, masterKey, kdfSalt, nil
}
