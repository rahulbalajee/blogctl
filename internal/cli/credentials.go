package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/rahulbalajee/blogctl/internal/client"
)

// maxCredentialsSize bounds what is read from a credentials file or FD.
const maxCredentialsSize = 4 << 10

type credentials struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// loadCredentials reads, in order: --credentials-fd, the BLOGCTL_CLIENT_ID
// and BLOGCTL_CLIENT_SECRET pair, then the credentials file. Errors never
// include credential values.
func (a *app) loadCredentials(ctx context.Context) (credentials, error) {
	if a.credentialsFD >= 0 && a.credentialsFD < 3 {
		return credentials{}, usagef("--credentials-fd must be 3 or higher; 0, 1 and 2 are stdin, stdout and stderr")
	}
	if a.credentialsFD >= 0 {
		open := a.env.OpenFD
		if open == nil {
			open = openFD
		}
		f, err := open(a.credentialsFD)
		if err != nil {
			return credentials{}, authError("cannot read --credentials-fd %d", a.credentialsFD)
		}
		defer f.Close()
		return parseCredentials(ctx, f, fmt.Sprintf("--credentials-fd %d", a.credentialsFD))
	}
	id, secret := a.env.Getenv("BLOGCTL_CLIENT_ID"), a.env.Getenv("BLOGCTL_CLIENT_SECRET")
	if id != "" || secret != "" {
		if id == "" || secret == "" {
			return credentials{}, authError("set both BLOGCTL_CLIENT_ID and BLOGCTL_CLIENT_SECRET, or neither")
		}
		return credentials{ClientID: id, ClientSecret: secret}, nil
	}
	path := a.credentialsFile
	if path == "" {
		var err error
		if path, err = a.defaultCredentialsFile(); err != nil {
			return credentials{}, err
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return credentials{}, authError("no credentials: use --credentials-fd, BLOGCTL_CLIENT_ID and BLOGCTL_CLIENT_SECRET, or a file at %s", path)
	}
	// Like ssh with private keys: refuse a file other users can read.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return credentials{}, authError("credentials file %s is readable by other users; run chmod 600 on it", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return credentials{}, authError("cannot read credentials file %s", path)
	}
	defer f.Close()
	return parseCredentials(ctx, f, path)
}

func (a *app) defaultCredentialsFile() (string, error) {
	if dir := a.env.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "blogctl", "credentials.json"), nil
	}
	home := a.env.HomeDir
	if home == nil {
		home = os.UserHomeDir
	}
	dir, err := home()
	if err != nil {
		return "", authError("no credentials file: cannot find the home directory")
	}
	return filepath.Join(dir, ".config", "blogctl", "credentials.json"), nil
}

func parseCredentials(ctx context.Context, r io.Reader, source string) (credentials, error) {
	data, err := readAllContext(ctx, r, maxCredentialsSize)
	if err != nil || len(data) > maxCredentialsSize {
		return credentials{}, authError("cannot read credentials from %s", source)
	}
	var creds credentials
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&creds); err != nil || creds.ClientID == "" || creds.ClientSecret == "" {
		return credentials{}, authError(`credentials in %s must be JSON {"clientId": "...", "clientSecret": "..."}`, source)
	}
	return creds, nil
}

func openFD(fd int) (io.ReadCloser, error) {
	f := os.NewFile(uintptr(fd), "credentials")
	if f == nil {
		return nil, fmt.Errorf("invalid file descriptor %d", fd)
	}
	return f, nil
}

func authError(format string, args ...any) error {
	return &client.Error{Kind: client.KindAuth, Message: fmt.Sprintf(format, args...)}
}
