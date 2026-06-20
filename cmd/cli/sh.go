package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

// PTY wire protocol (see pkg/handlers/shell_pty.go):
//
//	client → server:  binary [0x03 | stdin]    text {"type":"resize",...}
//	server → client:  binary [0x01 | stdout] [0x04 | replay] [0x05 | int32 exit]
const (
	frameStdout = 0x01
	frameStdin  = 0x03
	frameReplay = 0x04
	frameExit   = 0x05
)

// cmdSh implements `agentry sh [<sandbox>]` — an interactive shell into
// the sandbox's /workspace over the runtime PTY WebSocket, tunneled
// through the broker. The sandbox defaults to the current one.
func cmdSh(args []string) int {
	fs := flag.NewFlagSet("agentry sh", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sb := resolveSandbox(fs.Arg(0))
	if sb == "" {
		return die("usage: agentry sh [<sandbox>]\n(no sandbox given and no current sandbox set)")
	}
	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return die("agentry sh needs an interactive terminal")
	}

	cfg, sess, err := dialRuntime()
	if err != nil {
		return die("%v", err)
	}
	defer sess.Close()

	cols, rows := 80, 24
	if w, h, e := term.GetSize(stdinFd); e == nil && w > 0 && h > 0 {
		cols, rows = w, h
	}
	conn, err := dialRuntimeWS(sess, cfg.Cluster, sb,
		fmt.Sprintf("v1/shell/pty?session_id=%s&rows=%d&cols=%d", randTokenHex(8), rows, cols))
	if err != nil {
		return die("open shell: %v", err)
	}
	defer conn.Close()

	old, err := term.MakeRaw(stdinFd)
	if err != nil {
		return die("raw mode: %v", err)
	}
	defer term.Restore(stdinFd, old)

	// Resize on terminal-window change.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for range winch {
			w, h, e := term.GetSize(stdinFd)
			if e != nil {
				continue
			}
			msg, _ := json.Marshal(map[string]any{"type": "resize", "rows": h, "cols": w})
			_ = conn.WriteMessage(websocket.TextMessage, msg)
		}
	}()

	// Local stdin → PTY stdin frames.
	go func() {
		buf := make([]byte, 4096)
		for {
			n, e := os.Stdin.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage,
					append([]byte{frameStdin}, buf[:n]...)); werr != nil {
					return
				}
			}
			if e != nil {
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
		}
	}()

	// PTY output → local stdout, until the shell exits or the conn drops.
	exitCode := 0
	for {
		mt, data, e := conn.ReadMessage()
		if e != nil {
			break
		}
		if mt != websocket.BinaryMessage || len(data) < 1 {
			continue
		}
		switch data[0] {
		case frameStdout, frameReplay:
			os.Stdout.Write(data[1:])
		case frameExit:
			if len(data) >= 5 {
				exitCode = int(int32(binary.BigEndian.Uint32(data[1:5])))
			}
		}
	}
	return exitCode
}
