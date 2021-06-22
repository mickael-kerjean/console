package main

import (
	"context"
	"embed"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/gorilla/websocket"
	"github.com/kr/pty"
)

var (
	PORT int = 8080
	//go:embed public/index.html
	public embed.FS
)

func main() {
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", PORT),
		Handler: http.HandlerFunc(rootHandler),
	}
	done := make(chan os.Signal, 1)
	signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("[ERROR] %s\n", err.Error())
		}
	}()
	fmt.Printf("[INFO] server has started\n")
	<-done

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer func() {
		fmt.Printf("[INFO] cleaning up\n")
		cancel()
	}()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Printf("[ERROR] %s\n", err.Error())
	}
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			if f, err := public.Open("public/index.html"); err == nil {
				io.Copy(w, f)
			} else {
				w.Write([]byte("Oops"))
			}
			return
		}
	}
	if r.URL.Path == "/pty/socket" {
		handleSocket(w, r)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte("NOT FOUND"))
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}
var resizeMessage = struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
	X    uint16
	Y    uint16
}{}

func handleSocket(res http.ResponseWriter, req *http.Request) {
	conn, err := upgrader.Upgrade(res, req, nil)
	if err != nil {
		res.WriteHeader(http.StatusInternalServerError)
		res.Write([]byte("upgrade error"))
		return
	}
	defer conn.Close()

	var cmd *exec.Cmd
	if _, err = exec.LookPath("/bin/bash"); err == nil {
		bashCommand := `bash --noprofile --init-file <(cat <<EOF
export TERM="xterm"
export PS1="\[\033[1;34m\]\w\[\033[0;37m\] # \[\033[0m\]"
export EDITOR="emacs"`
		bashCommand += strings.Join([]string{
			"",
			"export PATH=" + os.Getenv("PATH"),
			"export HOME=" + os.Getenv("HOME"),
			"",
		}, "\n")
		bashCommand += `
alias ls='ls --color'
alias ll='ls -lah'
EOF
)`
		//cmd = exec.Command("/bin/bash", "-c", bashCommand)
		cmd = exec.Command("/bin/bash")
	} else if _, err = exec.LookPath("/bin/sh"); err == nil {
		cmd = exec.Command("/bin/sh")
		cmd.Env = []string{
			"TERM=xterm",
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + os.Getenv("HOME"),
		}
	} else {
		res.WriteHeader(http.StatusNotFound)
		res.Write([]byte("No terminal found"))
		return
	}

	tty, err := pty.Start(cmd)
	if err != nil {
		conn.WriteMessage(websocket.TextMessage, []byte(err.Error()))
		return
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Process.Wait()
		tty.Close()
	}()

	go func() {
		for {
			buf := make([]byte, 1024)
			read, err := tty.Read(buf)
			if err != nil {
				conn.WriteMessage(websocket.TextMessage, []byte(err.Error()))
				return
			}
			conn.WriteMessage(websocket.BinaryMessage, buf[:read])
		}
	}()

	for {
		messageType, reader, err := conn.NextReader()
		if err != nil {
			return
		} else if messageType == websocket.TextMessage {
			conn.WriteMessage(websocket.TextMessage, []byte("Unexpected text message"))
			continue
		}

		dataTypeBuf := make([]byte, 1)
		read, err := reader.Read(dataTypeBuf)
		if err != nil {
			conn.WriteMessage(websocket.TextMessage, []byte("Unable to read message type from reader"))
			return
		} else if read != 1 {
			return
		}

		switch dataTypeBuf[0] {
		case 0:
			if _, err := io.Copy(tty, reader); err != nil {
				conn.WriteMessage(websocket.TextMessage, []byte("Error copying bytes: "+err.Error()))
				continue
			}
		case 1:
			decoder := json.NewDecoder(reader)
			if err := decoder.Decode(&resizeMessage); err != nil {
				conn.WriteMessage(websocket.TextMessage, []byte("Error decoding resize message: "+err.Error()))
				continue
			}
			if _, _, errno := syscall.Syscall(
				syscall.SYS_IOCTL,
				tty.Fd(),
				syscall.TIOCSWINSZ,
				uintptr(unsafe.Pointer(&resizeMessage)),
			); errno != 0 {
				conn.WriteMessage(websocket.TextMessage, []byte("Unable to resize terminal: "+err.Error()))
			}
		default:
			conn.WriteMessage(websocket.TextMessage, []byte("Unknown data type: "+err.Error()))
		}
	}
}
