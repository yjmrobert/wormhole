package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/huh/v2"
	"golang.org/x/term"
)

func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func interactiveForm(input io.Reader, output io.Writer, fields ...huh.Field) *huh.Form {
	form := huh.NewForm(huh.NewGroup(fields...)).WithInput(input).WithOutput(output)
	if os.Getenv("NO_COLOR") != "" {
		form = form.WithTheme(huh.ThemeFunc(huh.ThemeBase))
	}
	return form
}

// confirm asks a yes/no question on the terminal.
func (a *App) confirm(prompt string) (bool, error) {
	in, inOK := a.Stdin.(*os.File)
	out, outOK := a.Stdout.(*os.File)
	if inOK && outOK && isTerminal(in) && isTerminal(out) {
		value := false
		err := interactiveForm(in, out, huh.NewConfirm().Title(prompt).Value(&value)).Run()
		if errors.Is(err, huh.ErrUserAborted) {
			return false, fail(ExitRefused, "not confirmed")
		}
		return value, err
	}
	fmt.Fprint(a.Stdout, prompt+" [y/N] ")
	r := bufio.NewReader(a.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// readSecret reads a password without echo when stdin is a terminal.
func (a *App) readSecret(c *common, prompt string) (string, error) {
	in, ok := a.Stdin.(*os.File)
	if ok && isTerminal(in) {
		if c.noInput {
			return "", fail(ExitRefused, "--no-input forbids password prompts; use --password-file or provide credentials on stdin")
		}
		out, outOK := a.Stdout.(*os.File)
		if !outOK || !isTerminal(out) {
			fmt.Fprint(a.Stdout, prompt)
			b, err := term.ReadPassword(int(in.Fd()))
			fmt.Fprintln(a.Stdout)
			return strings.TrimSpace(string(b)), err
		}
		value := ""
		field := huh.NewInput().Title(strings.TrimSpace(prompt)).EchoMode(huh.EchoModePassword).Value(&value)
		if err := interactiveForm(in, out, field).Run(); err != nil {
			if errors.Is(err, huh.ErrUserAborted) {
				return "", fail(ExitRefused, "password prompt aborted")
			}
			return "", err
		}
		return strings.TrimSpace(value), nil
	}
	fmt.Fprint(a.Stdout, prompt)
	r := bufio.NewReader(a.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
