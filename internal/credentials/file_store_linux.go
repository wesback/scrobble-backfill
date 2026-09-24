//go:build linux

package credentials

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	linuxCredentialDirectory = "credentials"
	linuxCredentialMagic     = "RSCRED01"
	linuxCredentialFileMode  = 0o600
	linuxCredentialDirMode   = 0o700
	linuxCredentialKeyFile   = ".encryption-key"
	linuxCredentialKeyLock   = ".encryption-key.lock"
	linuxCredentialKeySize   = 32
)

// LinuxFileStore stores encrypted credentials in separate owner-only files.
// Its key is derived from an owner-only random secret and bound to the Linux
// machine identity and the owning user.
type LinuxFileStore struct {
	Root            string
	machineIdentity func() (string, error)
	owningUserID    func() (string, error)
}

// NewLinuxFileStore creates a file store rooted at path.
func NewLinuxFileStore(path string) *LinuxFileStore {
	return &LinuxFileStore{
		Root:            path,
		machineIdentity: linuxMachineIdentity,
		owningUserID:    linuxOwningUserID,
	}
}

// LinuxDefaultPath returns the credential directory below the user's
// Rescrobble configuration directory.
func LinuxDefaultPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("determine credential directory: %w", err)
	}
	return filepath.Join(configDir, "rescrobble", linuxCredentialDirectory), nil
}

// NewDefaultLinuxFileStore creates a Linux file store at LinuxDefaultPath.
func NewDefaultLinuxFileStore() (*LinuxFileStore, error) {
	path, err := LinuxDefaultPath()
	if err != nil {
		return nil, err
	}
	return NewLinuxFileStore(path), nil
}

// Save encrypts and atomically replaces the credential for profile.
func (s *LinuxFileStore) Save(profile, session string) error {
	if err := validateProfile(profile); err != nil {
		return err
	}
	if strings.TrimSpace(session) == "" {
		return errors.New("Last.fm session credential must not be empty")
	}
	return s.withProfileLock(profile, func() error {
		encrypted, err := s.encrypt(profile, []byte(session))
		if err != nil {
			return err
		}
		return s.write(profile, encrypted)
	})
}

// Load decrypts and returns the credential for profile.
func (s *LinuxFileStore) Load(profile string) (string, error) {
	if err := validateProfile(profile); err != nil {
		return "", err
	}
	var session string
	err := s.withProfileLock(profile, func() error {
		data, err := readOwnerOnlyFile(s.pathFor(profile))
		if errors.Is(err, os.ErrNotExist) {
			return ErrCredentialNotFound
		}
		if err != nil {
			return fmt.Errorf("read credential for profile %q: %w", profile, err)
		}
		plaintext, err := s.decrypt(profile, data)
		if err != nil {
			return fmt.Errorf("decrypt credential for profile %q: %w", profile, err)
		}
		session = string(plaintext)
		return nil
	})
	return session, err
}

// Delete removes the credential for profile. Deleting an absent credential
// succeeds.
func (s *LinuxFileStore) Delete(profile string) error {
	if err := validateProfile(profile); err != nil {
		return err
	}
	return s.withProfileLock(profile, func() error {
		err := os.Remove(s.pathFor(profile))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("delete credential for profile %q: %w", profile, err)
		}
		return nil
	})
}

// Has reports whether profile has a saved credential.
func (s *LinuxFileStore) Has(profile string) (bool, error) {
	_, err := s.Load(profile)
	if errors.Is(err, ErrCredentialNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *LinuxFileStore) withProfileLock(profile string, operation func() error) error {
	if s == nil || strings.TrimSpace(s.Root) == "" {
		return errors.New("credential store path is empty")
	}
	if err := s.ensureRoot(); err != nil {
		return err
	}
	lock, err := acquireLinuxCredentialLock(s.lockPathFor(profile))
	if err != nil {
		return fmt.Errorf("lock credential for profile %q: %w", profile, err)
	}
	operationErr := operation()
	lockErr := lock.Close()
	return errors.Join(operationErr, lockErr)
}

func (s *LinuxFileStore) ensureRoot() error {
	if err := os.MkdirAll(s.Root, linuxCredentialDirMode); err != nil {
		return fmt.Errorf("create credential directory %q: %w", s.Root, err)
	}
	if err := os.Chmod(s.Root, linuxCredentialDirMode); err != nil {
		return fmt.Errorf("set credential directory permissions: %w", err)
	}
	return nil
}

func (s *LinuxFileStore) write(profile string, data []byte) error {
	temp, err := os.CreateTemp(s.Root, ".credential-*.tmp")
	if err != nil {
		return fmt.Errorf("create credential temporary file: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}()
	if err := temp.Chmod(linuxCredentialFileMode); err != nil {
		return fmt.Errorf("set credential permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write encrypted credential: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync encrypted credential: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close encrypted credential: %w", err)
	}
	if err := os.Rename(tempName, s.pathFor(profile)); err != nil {
		return fmt.Errorf("replace credential for profile %q: %w", profile, err)
	}
	directory, err := os.Open(s.Root)
	if err != nil {
		return fmt.Errorf("open credential directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync credential directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close credential directory: %w", err)
	}
	return nil
}

func (s *LinuxFileStore) encrypt(profile string, plaintext []byte) ([]byte, error) {
	encryptionKey, verificationKey, err := s.derivedKeys()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("initialize credential encryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize credential encryption: %w", err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate credential nonce: %w", err)
	}
	header := make([]byte, len(linuxCredentialMagic)+sha256.Size)
	copy(header, linuxCredentialMagic)
	copy(header[len(linuxCredentialMagic):], credentialKeyCheck(verificationKey, profile))
	sealed := aead.Seal(nil, nonce, plaintext, credentialAssociatedData(profile))
	output := make([]byte, 0, len(header)+len(nonce)+len(sealed))
	output = append(output, header...)
	output = append(output, nonce...)
	output = append(output, sealed...)
	return output, nil
}

func (s *LinuxFileStore) decrypt(profile string, data []byte) ([]byte, error) {
	headerSize := len(linuxCredentialMagic) + sha256.Size
	if len(data) < headerSize+12+16 || string(data[:len(linuxCredentialMagic)]) != linuxCredentialMagic {
		return nil, errors.New("invalid encrypted credential format")
	}
	encryptionKey, verificationKey, err := s.derivedKeys()
	if err != nil {
		return nil, err
	}
	expectedCheck := credentialKeyCheck(verificationKey, profile)
	actualCheck := data[len(linuxCredentialMagic):headerSize]
	if !hmac.Equal(actualCheck, expectedCheck) {
		return nil, ErrCredentialNotFound
	}
	block, err := aes.NewCipher(encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("initialize credential decryption: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize credential decryption: %w", err)
	}
	nonceEnd := headerSize + aead.NonceSize()
	if len(data) < nonceEnd+aead.Overhead() {
		return nil, errors.New("invalid encrypted credential format")
	}
	plaintext, err := aead.Open(nil, data[headerSize:nonceEnd], data[nonceEnd:], credentialAssociatedData(profile))
	if err != nil {
		return nil, errors.New("encrypted credential authentication failed")
	}
	return plaintext, nil
}

func (s *LinuxFileStore) derivedKeys() ([]byte, []byte, error) {
	if s == nil || s.machineIdentity == nil || s.owningUserID == nil {
		return nil, nil, errors.New("credential store identity is unavailable")
	}
	machineID, err := s.machineIdentity()
	if err != nil {
		return nil, nil, fmt.Errorf("read machine identity: %w", err)
	}
	userID, err := s.owningUserID()
	if err != nil {
		return nil, nil, fmt.Errorf("read owning Linux user identity: %w", err)
	}
	if machineID == "" || userID == "" {
		return nil, nil, errors.New("machine and user identities must not be empty")
	}
	secret, err := s.loadOrCreateSecret()
	if err != nil {
		return nil, nil, err
	}
	inputKeyMaterial := make([]byte, 0, len(secret)+len(machineID)+1+len(userID)+1)
	inputKeyMaterial = append(inputKeyMaterial, secret...)
	inputKeyMaterial = append(inputKeyMaterial, 0)
	inputKeyMaterial = append(inputKeyMaterial, machineID...)
	inputKeyMaterial = append(inputKeyMaterial, 0)
	inputKeyMaterial = append(inputKeyMaterial, userID...)
	keys := hkdfSHA256(inputKeyMaterial, []byte("rescrobble/linux-credential-store/v1"), []byte("AES-256-GCM and key verifier"), 64)
	return keys[:32], keys[32:], nil
}

func (s *LinuxFileStore) loadOrCreateSecret() ([]byte, error) {
	lock, err := acquireLinuxCredentialLock(filepath.Join(s.Root, linuxCredentialKeyLock))
	if err != nil {
		return nil, fmt.Errorf("lock credential encryption key: %w", err)
	}
	secret, operationErr := s.readOrCreateSecret()
	lockErr := lock.Close()
	if err := errors.Join(operationErr, lockErr); err != nil {
		return nil, err
	}
	return secret, nil
}

func (s *LinuxFileStore) readOrCreateSecret() ([]byte, error) {
	secretPath := filepath.Join(s.Root, linuxCredentialKeyFile)
	secret, err := readOwnerOnlyFile(secretPath)
	if err == nil {
		if len(secret) != linuxCredentialKeySize {
			return nil, errors.New("invalid credential encryption key file")
		}
		return secret, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read credential encryption key: %w", err)
	}

	secret = make([]byte, linuxCredentialKeySize)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return nil, fmt.Errorf("generate credential encryption key: %w", err)
	}
	temp, err := os.CreateTemp(s.Root, ".encryption-key-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temporary credential encryption key: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}()
	if err := temp.Chmod(linuxCredentialFileMode); err != nil {
		return nil, fmt.Errorf("set credential encryption key permissions: %w", err)
	}
	if _, err := temp.Write(secret); err != nil {
		return nil, fmt.Errorf("write credential encryption key: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return nil, fmt.Errorf("sync credential encryption key: %w", err)
	}
	if err := temp.Close(); err != nil {
		return nil, fmt.Errorf("close credential encryption key: %w", err)
	}
	if err := os.Link(tempName, secretPath); err != nil {
		return nil, fmt.Errorf("install credential encryption key: %w", err)
	}
	if err := syncCredentialDirectory(s.Root); err != nil {
		return nil, err
	}
	return secret, nil
}

func (s *LinuxFileStore) pathFor(profile string) string {
	return filepath.Join(s.Root, hex.EncodeToString([]byte(profile))+".enc")
}

func (s *LinuxFileStore) lockPathFor(profile string) string {
	return filepath.Join(s.Root, hex.EncodeToString([]byte(profile))+".lock")
}

func linuxMachineIdentity() (string, error) {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		data, err := os.ReadFile(path)
		if err == nil {
			identity := strings.TrimSpace(string(data))
			if identity == "" {
				return "", errors.New("machine identity is empty")
			}
			return identity, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", errors.New("machine identity file not found")
}

func linuxOwningUserID() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	if current.Uid == "" {
		return "", errors.New("current Linux user has no UID")
	}
	if _, err := strconv.ParseUint(current.Uid, 10, 32); err != nil {
		return "", fmt.Errorf("invalid current Linux user UID: %w", err)
	}
	return current.Uid, nil
}

func credentialKeyCheck(key []byte, profile string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("rescrobble credential key check\x00"))
	_, _ = mac.Write([]byte(profile))
	return mac.Sum(nil)
}

func credentialAssociatedData(profile string) []byte {
	associatedData := []byte("rescrobble/credential/v1\x00")
	return append(associatedData, profile...)
}

func syncCredentialDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open credential directory for sync: %w", err)
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return fmt.Errorf("sync credential directory: %w", err)
	}
	if err := directory.Close(); err != nil {
		return fmt.Errorf("close credential directory: %w", err)
	}
	return nil
}

func hkdfSHA256(inputKeyMaterial, salt, info []byte, length int) []byte {
	extract := hmac.New(sha256.New, salt)
	_, _ = extract.Write(inputKeyMaterial)
	pseudorandomKey := extract.Sum(nil)

	output := make([]byte, 0, length)
	var previousBlock []byte
	for counter := byte(1); len(output) < length; counter++ {
		expand := hmac.New(sha256.New, pseudorandomKey)
		_, _ = expand.Write(previousBlock)
		_, _ = expand.Write(info)
		_, _ = expand.Write([]byte{counter})
		previousBlock = expand.Sum(nil)
		output = append(output, previousBlock...)
	}
	return output[:length]
}

type linuxCredentialLock struct {
	file *os.File
}

func acquireLinuxCredentialLock(path string) (*linuxCredentialLock, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, linuxCredentialFileMode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := file.Chmod(linuxCredentialFileMode); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &linuxCredentialLock{file: file}, nil
}

func (lock *linuxCredentialLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}

func readOwnerOnlyFile(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("credential path is not a regular file")
	}
	if err := file.Chmod(linuxCredentialFileMode); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

var _ Store = (*LinuxFileStore)(nil)
