package tool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"unicode/utf8"
)

// shellSession owns one process and the output not yet returned to the model.
// The orchestrator serializes tool calls. The reader and waiter run separately.
type shellSession struct {
	process     *windowsProcess
	output      shellOutput
	inputClosed bool
	done        chan struct{}
	exitCode    int
	waitErr     error
}

type shellOutput struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
	finished  bool
}

func newShellSession(process *windowsProcess) *shellSession {
	session := &shellSession{process: process, done: make(chan struct{})}
	read := make(chan error, 1)
	go func() {
		_, err := io.Copy(&session.output, process.Output)
		process.Output.Close()
		read <- err
	}()
	go func() {
		defer close(session.done)
		code, err := process.Wait()
		process.Stdin.Close()
		session.exitCode = code
		session.waitErr = errors.Join(err, <-read)
		session.output.mu.Lock()
		session.output.finished = true
		session.output.mu.Unlock()
	}()
	return session
}

func (s *shellSession) send(ctx context.Context, chars string, closeStdin bool) error {
	if s.inputClosed {
		if chars == "" {
			return nil
		}
		return errors.New("shell stdin is already closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if chars != "" {
		encoded, err := encodeConsoleInput(chars)
		if err != nil {
			return err
		}
		write := make(chan error, 1)
		go func() {
			_, err := io.WriteString(s.process.Stdin, encoded)
			write <- err
		}()
		select {
		case err := <-write:
			if err != nil {
				return fmt.Errorf("write shell stdin: %w", err)
			}
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("send shell stdin: %w", ctx.Err()), s.stop())
		}
	}
	if closeStdin {
		s.inputClosed = true
		if err := s.process.Stdin.Close(); err != nil {
			select {
			case <-s.done:
			default:
				if !errors.Is(err, os.ErrClosed) {
					return fmt.Errorf("close shell stdin: %w", err)
				}
			}
		}
	}
	return nil
}

func (s *shellSession) stop() error {
	select {
	case <-s.done:
	default:
		if err := s.process.Kill(); err != nil {
			return err
		}
		<-s.done
	}
	if !s.process.treeStopped {
		return errors.Join(errors.New("Windows process tree termination was not confirmed"), s.waitErr)
	}
	return nil
}

func (out *shellOutput) Write(p []byte) (int, error) {
	out.mu.Lock()
	defer out.mu.Unlock()
	n := min(len(p), maxOutput-len(out.data))
	out.data = append(out.data, p[:n]...)
	out.truncated = out.truncated || n < len(p)
	return len(p), nil
}

func (out *shellOutput) take() (string, bool) {
	out.mu.Lock()
	defer out.mu.Unlock()
	n := len(out.data)
	if !out.finished && !out.truncated {
		// Preserve a partial UTF-8 rune for the next output chunk.
		start := n - 1
		for start >= 0 && n-start < utf8.UTFMax && !utf8.RuneStart(out.data[start]) {
			start--
		}
		if start >= 0 && !utf8.FullRune(out.data[start:]) {
			n = start
		}
	}
	text := decodeConsoleOutput(out.data[:n])
	out.data = out.data[:copy(out.data, out.data[n:])]
	truncated := out.truncated
	out.truncated = false
	return text, truncated
}
