package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/eugene-panin/kx/internal/store"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// UsageError is a mistake in how kx was called; main exits with 2 for it.
// Hint, if any, says how to call it right.
type UsageError struct {
	Err  error
	Hint string
}

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// ExitError carries a child's exit status up to main without an error message.
type ExitError int

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// bundle merges the given clusters into one kubeconfig with the first one as
// current-context. Namespaces switched in the target since the last change win.
func (a *App) bundle(refs []store.Ref) (*api.Config, error) {
	parts := make([]*api.Config, 0, len(refs))
	for _, r := range refs {
		cfg, err := a.Store.Get(r)
		if err != nil {
			return nil, err
		}
		parts = append(parts, cfg)
	}
	out := store.Merge(parts)
	out.CurrentContext = refs[0].String()
	if cur, err := a.loadTarget(); err == nil {
		for name, ctx := range out.Contexts {
			if tc := cur.Contexts[name]; tc != nil {
				ctx.Namespace = tc.Namespace
			}
		}
	}
	return out, nil
}

// Scope writes a temporary kubeconfig holding only the given clusters,
// disabled ones included. The caller must call cleanup when done with it.
func (a *App) Scope(args []string) (path string, cleanup func(), err error) {
	refs, err := a.Store.Expand(args)
	if err != nil {
		return "", nil, err
	}
	cfg, err := a.bundle(refs)
	if err != nil {
		return "", nil, err
	}
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return "", nil, err
	}
	// Named by pid in kx's own state dir, not in $TMPDIR: a kx killed with
	// -9 can't remove the file, and the next exec finds and removes it.
	dir := filepath.Join(a.stateDir, "exec")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	removeOrphans(dir)
	f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("kx-%d.yaml", os.Getpid())), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { os.Remove(f.Name()) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return f.Name(), cleanup, nil
}

// removeOrphans deletes scoped kubeconfigs whose kx is gone.
func removeOrphans(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		var pid int
		if _, err := fmt.Sscanf(e.Name(), "kx-%d.yaml", &pid); err != nil {
			continue
		}
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func ScopeEnv(path string, args []string) []string {
	return append(os.Environ(), "KUBECONFIG="+path, "KX_SCOPE="+strings.Join(args, ","))
}

// Exec runs command with KUBECONFIG pointing at a temporary kubeconfig that
// holds only the given clusters, disabled ones included.
func (a *App) Exec(args, command []string) error {
	path, cleanup, err := a.Scope(args)
	if err != nil {
		return err
	}
	defer cleanup()

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.Stdin, a.Stdout, a.Stderr
	cmd.Env = ScopeEnv(path, args)
	if err := cmd.Start(); err != nil {
		// What a shell would say and exit with [B2].
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(a.Stderr, "kx: %s: command not found\n", command[0])
			return ExitError(127)
		}
		fmt.Fprintf(a.Stderr, "kx: %s: %v\n", command[0], err)
		return ExitError(126)
	}

	// Stay alive until the child exits so the temp file gets removed. Ctrl-C
	// already reaches the child through the terminal's process group; other
	// termination signals are forwarded.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s != os.Interrupt {
					cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	signal.Stop(sigs)
	close(done)

	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
		if code < 0 {
			code = 1
		}
		return ExitError(code)
	}
	return err
}
