package crypto

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"io"
	"testing"
)

func TestEncryptDecryptBlocksAndRandomSeek(t *testing.T) {
	salt, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	key := DeriveMasterKey("123456", salt)
	if len(key) != KeySize {
		t.Fatalf("expected key size %d, got %d", KeySize, len(key))
	}

	// Span 3 full blocks + partial 4th block.
	plain := make([]byte, PlainBlockSize*3+12345)
	if _, err := io.ReadFull(rand.Reader, plain); err != nil {
		t.Fatal(err)
	}

	er, hdr, err := NewEncryptReader(bytes.NewReader(plain), key)
	if err != nil {
		t.Fatal(err)
	}
	// Read with awkward small buffer size to stress coalescing & buffering.
	var cipherBuf bytes.Buffer
	tmp := make([]byte, 4093)
	for {
		n, err := er.Read(tmp)
		if n > 0 {
			cipherBuf.Write(tmp[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	ciphertext := cipherBuf.Bytes()
	wantSize := CipherSize(int64(len(plain)))
	if int64(len(ciphertext)) != wantSize {
		t.Fatalf("cipher size mismatch: got %d want %d", len(ciphertext), wantSize)
	}
	if !bytes.Equal(ciphertext[:PartHeaderSize], hdr[:]) {
		t.Fatal("header prefix mismatch")
	}

	dec, err := NewPartDecryptor(key, hdr[:])
	if err != nil {
		t.Fatal(err)
	}

	// Random-access decrypt in reverse block order.
	for _, blkIdx := range []uint64{3, 1, 0, 2} {
		off, length, err := BlockCipherBounds(int64(len(plain)), blkIdx)
		if err != nil {
			t.Fatalf("block %d bounds: %v", blkIdx, err)
		}
		got, err := dec.DecryptBlock(blkIdx, ciphertext[off:off+length], nil)
		if err != nil {
			t.Fatalf("block %d decrypt: %v", blkIdx, err)
		}
		pStart := int(blkIdx) * PlainBlockSize
		pEnd := pStart + PlainBlockSize
		if pEnd > len(plain) {
			pEnd = len(plain)
		}
		if !bytes.Equal(got, plain[pStart:pEnd]) {
			t.Fatalf("block %d plaintext mismatch", blkIdx)
		}
	}

	// Tamper detection: flipping 1 bit or swapping block index must fail.
	off0, len0, _ := BlockCipherBounds(int64(len(plain)), 0)
	if _, err := dec.DecryptBlock(1, ciphertext[off0:off0+len0], nil); err == nil {
		t.Fatal("expected AAD/nonce mismatch when decrypting block 0 as block 1")
	}
	tampered := append([]byte(nil), ciphertext[off0:off0+len0]...)
	tampered[10] ^= 0xFF
	if _, err := dec.DecryptBlock(0, tampered, nil); err == nil {
		t.Fatal("expected tamper failure")
	}
}

func TestDBBackupRoundTripAndWrongPassphrase(t *testing.T) {
	salt, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	passphrase := "123456"
	key := DeriveMasterKey(passphrase, salt)

	fakeSQLite := append([]byte("SQLite format 3\x00"), bytes.Repeat([]byte("telemount-meta-row-"), 500)...)
	blob, err := EncryptDBBackup(fakeSQLite, key, salt)
	if err != nil {
		t.Fatal(err)
	}

	gotSQLite, gotKey, gotSalt, err := DecryptDBBackup(blob, passphrase)
	if err != nil {
		t.Fatalf("decrypt backup: %v", err)
	}
	if !bytes.Equal(gotSQLite, fakeSQLite) {
		t.Fatal("restored sqlite bytes mismatch")
	}
	if !VerifyKeyEqual(gotKey, key) || !bytes.Equal(gotSalt, salt) {
		t.Fatal("derived key or salt mismatch")
	}

	if _, _, _, err := DecryptDBBackup(blob, "wrong-pass"); err == nil {
		t.Fatal("expected wrong passphrase to fail decryption")
	}
}

func TestLegacyTeleDrivePartAndBackupCompat(t *testing.T) {
	salt, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	passphrase := "legacy-pass"
	key := DeriveMasterKey(passphrase, salt)

	// 1. Legacy TDP1 part block (HKDF info "teledrive-part-v1", AAD prefix "TDP1").
	var hdr [PartHeaderSize]byte
	copy(hdr[0:4], legacyPartHeaderMagic[:])
	_, _ = io.ReadFull(rand.Reader, hdr[4:32])
	partKey, err := derivePartKeyWithInfo(key, hdr[4:20], "teledrive-part-v1")
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := aes.NewCipher(partKey)
	aead, _ := cipher.NewGCM(blk)
	var nonce [NonceSize]byte
	var aad [12]byte
	blockNonce(hdr[20:32], 0, nonce[:])
	blockAADWithMagic(legacyPartHeaderMagic, 0, aad[:])
	wantPlain := []byte("legacy teledrive encrypted payload")
	ct := aead.Seal(nil, nonce[:], wantPlain, aad[:])

	dec, err := NewPartDecryptor(key, hdr[:])
	if err != nil {
		t.Fatalf("NewPartDecryptor legacy TDP1: %v", err)
	}
	gotPlain, err := dec.DecryptBlock(0, ct, nil)
	if err != nil || !bytes.Equal(gotPlain, wantPlain) {
		t.Fatalf("legacy TDP1 decrypt = (%q, %v), want %q", gotPlain, err, wantPlain)
	}

	// 2. Legacy TDDB backup blob.
	var gzBuf bytes.Buffer
	gz := gzip.NewWriter(&gzBuf)
	wantSQLite := []byte("SQLite format 3\x00legacy-db")
	_, _ = gz.Write(wantSQLite)
	_ = gz.Close()

	var bhdr [dbBackupHeaderSize]byte
	copy(bhdr[0:4], legacyDBBackupMagic[:])
	bhdr[4] = 1
	binary.BigEndian.PutUint32(bhdr[5:9], ArgonTime)
	binary.BigEndian.PutUint32(bhdr[9:13], ArgonMemory)
	bhdr[13] = ArgonThreads
	copy(bhdr[14:30], salt)
	_, _ = io.ReadFull(rand.Reader, bhdr[30:42])
	mb, _ := aes.NewCipher(key)
	maead, _ := cipher.NewGCM(mb)
	legacyBlob := append([]byte(nil), bhdr[:]...)
	legacyBlob = maead.Seal(legacyBlob, bhdr[30:42], gzBuf.Bytes(), bhdr[:])

	gotSQLite, gotKey, gotSalt, err := DecryptDBBackup(legacyBlob, passphrase)
	if err != nil {
		t.Fatalf("DecryptDBBackup legacy TDDB: %v", err)
	}
	if !bytes.Equal(gotSQLite, wantSQLite) || !VerifyKeyEqual(gotKey, key) || !bytes.Equal(gotSalt, salt) {
		t.Fatal("legacy TDDB restored content/key/salt mismatch")
	}
}
