package home

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	identityFile = "identity.key"
	nameFile     = "name"
	listenFile   = "listen.url"
	dbFile       = "string-can.db"
	lockFile     = "serve.lock"
	DefaultName  = "string-can"
)

// Dir is a string-can home directory.
type Dir struct {
	Path string
}

// Resolve returns STRING_CAN_HOME or ~/.string-can.
func Resolve() (Dir, error) {
	if p := os.Getenv("STRING_CAN_HOME"); p != "" {
		return Dir{Path: p}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Dir{}, err
	}
	return Dir{Path: filepath.Join(home, ".string-can")}, nil
}

func (d Dir) file(name string) string {
	return filepath.Join(d.Path, name)
}

// DBPath returns the SQLite database path.
func (d Dir) DBPath() string {
	return d.file(dbFile)
}

// Ensure creates the home directory if needed.
func (d Dir) Ensure() error {
	return os.MkdirAll(d.Path, 0o700)
}

// Identity loads or creates identity.key (mode 0600).
func (d Dir) Identity() (ed25519.PrivateKey, error) {
	if err := d.Ensure(); err != nil {
		return nil, err
	}
	path := d.file(identityFile)
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("invalid identity.key")
		}
		return ed25519.PrivateKey(data), nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			if len(data) != ed25519.PrivateKeySize {
				return nil, fmt.Errorf("invalid identity.key")
			}
			return ed25519.PrivateKey(data), nil
		}
		return nil, err
	}
	if _, err := f.Write(priv); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return priv, nil
}

// HasName reports whether a display name file exists.
func (d Dir) HasName() bool {
	_, err := os.Stat(d.file(nameFile))
	return err == nil
}

// Name returns the stored display name, or DefaultName if none is stored.
func (d Dir) Name() (string, error) {
	b, err := os.ReadFile(d.file(nameFile))
	if os.IsNotExist(err) {
		return DefaultName, nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// SetName writes the display name.
func (d Dir) SetName(name string) error {
	if err := d.Ensure(); err != nil {
		return err
	}
	return os.WriteFile(d.file(nameFile), []byte(name), 0o644)
}

// ListenURL returns the URL written when serve last bound.
func (d Dir) ListenURL() (string, error) {
	b, err := os.ReadFile(d.file(listenFile))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// SetListenURL writes listen.url with no trailing slash.
func (d Dir) SetListenURL(u string) error {
	if err := d.Ensure(); err != nil {
		return err
	}
	return os.WriteFile(d.file(listenFile), []byte(u), 0o644)
}

// Lock is an exclusive flock on the home directory.
type Lock struct {
	f *os.File
}

// Lock acquires an exclusive non-blocking lock for serve.
func (d Dir) Lock() (*Lock, error) {
	if err := d.Ensure(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(d.file(lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another string-can server is already using %s", d.Path)
	}
	return &Lock{f: f}, nil
}

// Unlock releases the serve lock.
func (l *Lock) Unlock() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
