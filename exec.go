package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/clientcmd/api"
)

// exitError carries a child's exit status up to main without an error message.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// bundle merges the given clusters into one kubeconfig with the first one as
// current-context. Namespaces switched in the target since the last change win.
func (a *app) bundle(refs []ref) (*api.Config, error) {
	parts := make([]*api.Config, 0, len(refs))
	for _, r := range refs {
		cfg, err := a.store.get(r)
		if err != nil {
			return nil, err
		}
		parts = append(parts, cfg)
	}
	out := merge(parts)
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

// exec runs command with KUBECONFIG pointing at a temporary kubeconfig that
// holds only the given clusters, disabled ones included.
func (a *app) exec(args, command []string) error {
	refs, err := a.store.expand(args)
	if err != nil {
		return err
	}
	cfg, err := a.bundle(refs)
	if err != nil {
		return err
	}
	data, err := clientcmd.Write(*cfg)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "kx-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = a.stdin, a.stdout, a.stderr
	cmd.Env = append(os.Environ(), "KUBECONFIG="+f.Name(), "KX_SCOPE="+strings.Join(args, ","))
	if err := cmd.Start(); err != nil {
		return err
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
		return exitError(code)
	}
	return err
}
