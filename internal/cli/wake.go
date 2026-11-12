package cli

import (
	"fmt"

	"github.com/supersuit-tech/string-can/internal/canon"
	"github.com/supersuit-tech/string-can/internal/store"
	"github.com/supersuit-tech/string-can/internal/wake"
)

func (e env) cmdWake(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, commandHelp("wake"))
		return 2
	}
	switch args[0] {
	case "set":
		return e.cmdWakeSet(args[1:])
	case "show":
		return e.cmdWakeShow(args[1:])
	case "clear":
		return e.cmdWakeClear(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(e.stdout, commandHelp("wake"))
		return 0
	default:
		return e.fail(fmt.Errorf("unknown wake command %q\n%s", args[0], commandHelp("wake")))
	}
}

func (e env) cmdWakeSet(args []string) int {
	var file, envName, literal string
	fs := newFlagSet("wake set")
	fs.StringVar(&file, "token-file", "", "file containing the bearer token")
	fs.StringVar(&envName, "token-env", "", "environment variable holding the bearer token")
	fs.StringVar(&literal, "token", "", "bearer token (stored in shell history; prefer --token-file or --token-env)")
	pos, help, err := parseWith(fs, args, map[string]bool{
		"token-file": true,
		"token-env":  true,
		"token":      true,
	})
	if help {
		fmt.Fprint(e.stdout, commandHelp("wake"))
		return 0
	}
	if err != nil {
		return e.fail(err)
	}
	if len(pos) != 2 {
		return e.fail(fmt.Errorf("usage: string-can wake set <public-key> <url> [--token-file <path> | --token-env <NAME> | --token <literal>]"))
	}
	key, err := canon.NormalizeKey(pos[0])
	if err != nil {
		return e.fail(err)
	}
	wakeURL, err := wake.NormalizeURL(pos[1])
	if err != nil {
		return e.fail(err)
	}
	token, warning, err := wake.LoadToken(file, envName, literal)
	if err != nil {
		return e.fail(err)
	}
	if warning != "" {
		fmt.Fprintln(e.stderr, "string-can: "+warning)
	}
	st, _, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	// Omitting a token source leaves a token that was already stored.
	// Pass --token-file, --token-env, or --token to replace it.
	if file == "" && envName == "" && literal == "" {
		if _, existing, lerr := st.LoadWake(key); lerr == nil {
			token = existing
		}
	}
	if err := st.SetWake(key, wakeURL, token); err != nil {
		return e.fail(err)
	}
	c, err := st.GetContact(key)
	if err != nil {
		return e.fail(err)
	}
	return e.writeJSON(wakeView(c))
}

func (e env) cmdWakeShow(args []string) int {
	key, code := e.oneKey("wake show", args)
	if code >= 0 {
		return code
	}
	st, _, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	c, err := st.GetContact(key)
	if err != nil {
		return e.fail(err)
	}
	return e.writeJSON(wakeView(c))
}

func (e env) cmdWakeClear(args []string) int {
	key, code := e.oneKey("wake clear", args)
	if code >= 0 {
		return code
	}
	st, _, err := openStore()
	if err != nil {
		return e.fail(err)
	}
	defer st.Close()
	if err := st.ClearWake(key); err != nil {
		return e.fail(err)
	}
	c, err := st.GetContact(key)
	if err != nil {
		return e.fail(err)
	}
	return e.writeJSON(wakeView(c))
}

// oneKey parses a wake subcommand that takes only a public key.
// A non-negative return is an exit code. -1 means key is set.
func (e env) oneKey(name string, args []string) (string, int) {
	_, pos, help, err := flags(name, args, nil)
	if help {
		fmt.Fprint(e.stdout, commandHelp("wake"))
		return "", 0
	}
	if err != nil {
		return "", e.fail(err)
	}
	if len(pos) != 1 {
		return "", e.fail(fmt.Errorf("usage: string-can %s <public-key>", name))
	}
	key, err := canon.NormalizeKey(pos[0])
	if err != nil {
		return "", e.fail(err)
	}
	return key, -1
}

func wakeView(c store.Contact) struct {
	PublicKey string `json:"public_key"`
	WakeURL   string `json:"wake_url"`
	WakeToken string `json:"wake_token"`
} {
	token := ""
	if c.WakeTokenSet {
		token = wake.MaskedToken
	}
	return struct {
		PublicKey string `json:"public_key"`
		WakeURL   string `json:"wake_url"`
		WakeToken string `json:"wake_token"`
	}{
		PublicKey: c.PublicKey,
		WakeURL:   wake.MaskURL(c.WakeURL),
		WakeToken: token,
	}
}
