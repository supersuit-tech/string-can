// Package files stores attachment bytes under STRING_CAN_HOME.
// Blobs are content-addressed. A download copies a verified blob to a
// sanitized file name. Nothing in this package executes file contents.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/supersuit-tech/string-can/internal/canon"
)

const DefaultMaxBytes int64 = 25 << 20

// HashMismatch means bytes were not saved because the digest did not match
// the signed SHA-256.
type HashMismatch struct {
	Name string
}

func (e *HashMismatch) Error() string {
	if e == nil || e.Name == "" {
		return "hash mismatch"
	}
	return "hash mismatch for " + e.Name
}

// Store is the content-addressed blob directory.
type Store struct {
	Dir string
}

// Open creates the blob directory (mode 0700) under home.
func Open(home string) (*Store, error) {
	dir := filepath.Join(home, "blobs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

// MaxBytes returns STRING_CAN_MAX_FILE_BYTES, or 25 MiB when it is unset.
func MaxBytes() (int64, error) {
	raw := strings.TrimSpace(os.Getenv("STRING_CAN_MAX_FILE_BYTES"))
	if raw == "" {
		return DefaultMaxBytes, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("STRING_CAN_MAX_FILE_BYTES must be a non-negative integer")
	}
	if n > canon.ProtocolMaxFileBytes {
		return 0, fmt.Errorf("STRING_CAN_MAX_FILE_BYTES is over the %d byte protocol limit", canon.ProtocolMaxFileBytes)
	}
	return n, nil
}

// Ingest copies path into the blob directory and returns its signed metadata.
// The signed name is the base name, so a path cannot travel with the file.
func (s *Store) Ingest(path string, max int64) (canon.File, error) {
	name, err := canon.SafeName(filepath.Base(path))
	if err != nil {
		return canon.File{}, fmt.Errorf("%s: invalid file name", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return canon.File{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return canon.File{}, err
	}
	if info.IsDir() {
		return canon.File{}, fmt.Errorf("%s is a directory", path)
	}
	if info.Size() > max {
		return canon.File{}, tooLarge(name, info.Size(), max)
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return canon.File{}, err
	}
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return canon.File{}, err
	}
	sum, size, err := s.put(f, max)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			return canon.File{}, tooLarge(name, size, max)
		}
		return canon.File{}, err
	}
	return canon.File{
		Name:   name,
		Size:   size,
		Type:   DetectType(name, head),
		SHA256: sum,
	}, nil
}

// SaveVerified writes r only when its size and SHA-256 match the signed
// values. A mismatch deletes the temporary copy and does not replace a blob
// already stored under the expected hash.
func (s *Store) SaveVerified(name, wantSHA string, wantSize int64, r io.Reader) error {
	wantSHA, err := canon.NormalizeSHA(wantSHA)
	if err != nil {
		return err
	}
	if wantSize < 0 || wantSize > canon.ProtocolMaxFileBytes {
		return fmt.Errorf("invalid file size")
	}
	tmp, sum, size, err := s.writeTemp(io.LimitReader(r, wantSize+1), wantSize)
	if tmp != "" {
		defer os.Remove(tmp)
	}
	if err != nil && !errors.Is(err, errTooLarge) {
		return err
	}
	if sum != wantSHA || size != wantSize {
		return &HashMismatch{Name: name}
	}
	return s.commit(tmp, sum)
}

// Open opens a stored blob by hash. The hash is restricted to hex.
func (s *Store) Open(sha string) (io.ReadCloser, error) {
	sha, err := canon.NormalizeSHA(sha)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(s.Dir, sha))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("file blob %s is not stored", sha[:8])
		}
		return nil, err
	}
	return f, nil
}

// Has reports whether the blob is stored.
func (s *Store) Has(sha string) bool {
	sha, err := canon.NormalizeSHA(sha)
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(s.Dir, sha))
	return err == nil && !info.IsDir()
}

// Materialize copies a verified blob to dest. dest must already be a safe path.
// The copy is mode 0600 and is not marked executable. Bytes are hashed again
// before the destination is replaced.
func (s *Store) Materialize(name, sha string, size int64, dest string) error {
	sha, err := canon.NormalizeSHA(sha)
	if err != nil {
		return err
	}
	src, err := s.Open(sha)
	if err != nil {
		return err
	}
	defer src.Close()
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), src)
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != sha || n != size {
		return &HashMismatch{Name: name}
	}
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return err
	}
	committed = true
	return os.Chmod(dest, 0o600)
}

// SavedPath is STRING_CAN_HOME/files/<peer>/<id>/<name>. name is checked
// again so a signed name cannot escape that directory.
func SavedPath(home, peer string, id int64, name string) (string, error) {
	peer, err := canon.NormalizeKey(peer)
	if err != nil {
		return "", err
	}
	name, err = canon.SafeName(name)
	if err != nil {
		return "", err
	}
	if id < 0 {
		return "", fmt.Errorf("invalid message id")
	}
	dir := filepath.Join(home, "files", peer, strconv.FormatInt(id, 10))
	dest := filepath.Join(dir, name)
	rel, err := filepath.Rel(dir, dest)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid file name")
	}
	return dest, nil
}

// DetectType picks a bare content type from the extension, then the bytes.
func DetectType(name string, head []byte) string {
	if ext := filepath.Ext(name); ext != "" {
		if t := bareType(mime.TypeByExtension(ext)); t != "" && canon.CheckType(t) == nil {
			return t
		}
	}
	if t := bareType(http.DetectContentType(head)); t != "" && canon.CheckType(t) == nil {
		return t
	}
	return "application/octet-stream"
}

func bareType(t string) string {
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(strings.ToLower(t))
}

var errTooLarge = errors.New("file too large")

func tooLarge(name string, size, max int64) error {
	return fmt.Errorf("%s is %s, over the %s limit (STRING_CAN_MAX_FILE_BYTES)", name, fmtBytes(size), fmtBytes(max))
}

func fmtBytes(n int64) string {
	const kb = 1024
	const mb = 1024 * kb
	switch {
	case n >= mb && n%mb == 0:
		return fmt.Sprintf("%d MB", n/mb)
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n == 1:
		return "1 byte"
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// put streams r into the blob directory, stopping after max+1 bytes.
func (s *Store) put(r io.Reader, max int64) (sum string, size int64, err error) {
	tmp, sum, size, err := s.writeTemp(r, max)
	if tmp != "" {
		defer os.Remove(tmp)
	}
	if err != nil {
		return sum, size, err
	}
	if err := s.commit(tmp, sum); err != nil {
		return "", size, err
	}
	return sum, size, nil
}

// writeTemp hashes r into a private temp file. The caller removes tmpName
// unless commit renamed it.
func (s *Store) writeTemp(r io.Reader, max int64) (tmpName, sum string, size int64, err error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return "", "", 0, err
	}
	tmp, err := os.CreateTemp(s.Dir, ".incoming-*")
	if err != nil {
		return "", "", 0, err
	}
	tmpName = tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return tmpName, "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, max+1))
	if err != nil {
		tmp.Close()
		return tmpName, "", n, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return tmpName, "", n, err
	}
	if err := tmp.Close(); err != nil {
		return tmpName, "", n, err
	}
	if n > max {
		return tmpName, "", n, errTooLarge
	}
	return tmpName, hex.EncodeToString(h.Sum(nil)), n, nil
}

// commit moves a verified temp file to its content hash. An existing blob
// with the same hash is left in place.
func (s *Store) commit(tmpName, sum string) error {
	sum, err := canon.NormalizeSHA(sum)
	if err != nil {
		return err
	}
	dest := filepath.Join(s.Dir, sum)
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return err
	}
	return os.Chmod(dest, 0o600)
}
