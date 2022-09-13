package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	flags "github.com/jessevdk/go-flags"
	"github.com/scrapli/scrapligo/driver/network"
	"github.com/scrapli/scrapligo/driver/options"
	"github.com/scrapli/scrapligo/platform"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/term"
)

const (
	TRANSPORT_AUTO TransportType = iota
	TRANSPORT_SSH
	TRANSPORT_TELNET
)

type TransportType int

func (tt TransportType) String() string {
	return [...]string{
		"auto",
		"ssh",
		"telnet",
	}[tt]
}

func TransportFromName(name string) TransportType {
	return map[string]TransportType{
		"auto":   TRANSPORT_AUTO,
		"ssh":    TRANSPORT_SSH,
		"telnet": TRANSPORT_TELNET,
	}[name]
}

type XR struct {
	addr      string
	logger    *log.Logger
	transport TransportType

	*network.Driver
}

func NewXR(addr string, transport TransportType) *XR {
	prefix := fmt.Sprintf("[%s] ", ellipsis(addr, 25))
	logger := log.New(os.Stdout, prefix, log.LstdFlags|log.Lmsgprefix)

	return &XR{
		addr:      addr,
		logger:    logger,
		transport: transport,
	}
}

func (xr *XR) Connect(cred *Credential) error {
	if xr.transport == TRANSPORT_AUTO {
		xr.transport = TRANSPORT_SSH
		if err := xr.connect(cred); err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				xr.logger.Printf("Connection failed: %+v", err)
				xr.logger.Printf("Fallback connection from ssh to telnet.")
				xr.transport = TRANSPORT_TELNET
				return xr.connect(cred)
			}
			return err
		}
	}
	return xr.connect(cred)
}

func (xr *XR) connect(cred *Credential) error {
	xr.logger.Printf("Trying %s...", xr.addr)
	pf, err := xr.createPlatform(cred)
	if err != nil {
		return fmt.Errorf("%s: %w\n", xr.transport, err)
	}

	xr.Driver, err = pf.GetNetworkDriver()
	if err != nil {
		return fmt.Errorf("%s: %w\n", xr.transport, err)
	}

	if err := xr.Open(); err != nil {
		return fmt.Errorf("%s: %w\n", xr.transport, err)
	}

	xr.logger.Printf("Connected to %s.", xr.addr)
	return nil
}

func (xr *XR) execute(command string, withPrompt bool) ([]string, error) {
	lines := []string{}

	if withPrompt {
		prompt, err := xr.GetPrompt()
		if err != nil {
			return nil, err
		}

		newLine := fmt.Sprintf("%s%s", strings.TrimSpace(prompt), command)
		lines = append(lines, "", newLine)
	}

	resp, err := xr.SendCommand(command)
	if err != nil {
		return nil, err
	}

	lines = append(lines, strings.Split(resp.Result, "\n")...)
	return lines, nil
}

func (xr *XR) createPlatform(cred *Credential) (*platform.Platform, error) {
	switch xr.transport {
	case TRANSPORT_SSH:
		host, port := xr.getHostPort(22)
		return platform.NewPlatform(
			platform.CiscoIosxr, host,
			options.WithAuthNoStrictKey(),
			options.WithAuthUsername(cred.Username),
			options.WithAuthPassword(cred.Password),
			options.WithPort(port),
			options.WithTransportType("standard"),
		)
	case TRANSPORT_TELNET:
		host, port := xr.getHostPort(23)
		return platform.NewPlatform(
			platform.CiscoIosxr, host,
			options.WithAuthUsername(cred.Username),
			options.WithAuthPassword(cred.Password),
			options.WithPort(port),
			options.WithTransportType("telnet"),
		)
	}
	return nil, fmt.Errorf("transport not found")
}

func (xr *XR) getHostPort(defaultPort int) (string, int) {
	host, portStr, err := net.SplitHostPort(xr.addr)
	if err != nil {
		return xr.addr, defaultPort
	}

	port, err := strconv.Atoi(portStr)
	if err != nil {
		return host, defaultPort
	}

	return host, port
}

type Credential struct {
	Username string
	Password string
}

func NewCredential(user string) (*Credential, error) {
	if user == "" {
		return nil, fmt.Errorf("Username not specified")
	}

	userinfo := strings.SplitN(user, ":", 2)
	if len(userinfo) == 1 {
		password, err := readPassword("Password: ")
		if err != nil {
			return nil, err
		}
		userinfo = append(userinfo, password)
	}

	return &Credential{userinfo[0], userinfo[1]}, nil
}

func readPassword(prompt string) (string, error) {
	signalChan := make(chan os.Signal)
	signal.Notify(signalChan, os.Interrupt)
	defer signal.Stop(signalChan)

	cs, err := terminal.GetState(int(syscall.Stdin))
	if err != nil {
		return "", err
	}

	go func() {
		<-signalChan
		terminal.Restore(int(syscall.Stdin), cs)
		os.Exit(0)
	}()

	fmt.Print(prompt)
	tty, err := os.Open("/dev/tty")
	if err != nil {
		log.Fatal(err)
	}
	defer tty.Close()

	password, err := term.ReadPassword(int(tty.Fd()))
	if err != nil {
		return "", err
	}

	fmt.Println()
	return string(password), nil
}

func ellipsis(text string, length int) string {
	r := []rune(text)
	if len(r) > length {
		return string(r[0:length])
	}
	return text
}

type WorkerPool struct {
	wg   sync.WaitGroup
	jobs chan func()
}

func NewWorkPool(size int) *WorkerPool {
	wp := &WorkerPool{
		jobs: make(chan func()),
		wg:   sync.WaitGroup{},
	}

	for i := 0; i < size; i++ {
		wp.wg.Add(1)
		go func() {
			defer wp.wg.Done()
			for fn := range wp.jobs {
				fn()
			}
		}()
	}

	return wp
}

func (wp *WorkerPool) Go(fn func()) {
	wp.jobs <- fn
}

func (wp *WorkerPool) Wait() {
	close(wp.jobs)
	wp.wg.Wait()
}

func main() {
	var opts struct {
		Commands    []string `short:"e" long:"exec" description:"Commandline arguments"`
		Concurrency int      `short:"j" long:"jobs" description:"Concurrency size" default:"5"`
		Userinfo    string   `short:"u" long:"user" description:"Specify login username" env:"USERINFO"`
		JsonFormat  bool     `long:"json" description:"Output results in JSON format"`
		Transport   string   `short:"t" description:"Set transport protocol" choice:"ssh" choice:"telnet"`
	}

	addrs, err := flags.ParseArgs(&opts, os.Args[1:])
	if err != nil {
		if fe, ok := err.(*flags.Error); ok && fe.Type == flags.ErrHelp {
			os.Exit(0)
		}
		log.Fatal(err)
	}

	cred, err := NewCredential(opts.Userinfo)
	if err != nil {
		log.Fatalf("Error: %+v\n", err)
	}

	mu := new(sync.Mutex)
	wp := NewWorkPool(opts.Concurrency)
	results := make(map[string]map[string][]string, len(addrs))

	for i := range addrs {
		addr := addrs[i]
		results[addr] = make(map[string][]string, len(opts.Commands))

		wp.Go(func() {
			xr := NewXR(addr, TransportFromName(opts.Transport))
			if opts.JsonFormat {
				xr.logger.SetOutput(ioutil.Discard)
			}

			if err := xr.Connect(cred); err != nil {
				xr.logger.Printf("Connection failed: %+v", err)
				return
			}
			defer xr.Close()

			for _, command := range opts.Commands {
				lines, err := xr.execute(command, !opts.JsonFormat)
				if err != nil {
					xr.logger.Printf("Failed to send command: %+v\n", err)
					continue
				}

				for _, line := range lines {
					xr.logger.Printf(line)
				}

				if opts.JsonFormat {
					mu.Lock()
					results[addr][command] = lines
					mu.Unlock()
				}

			}
		})
	}

	wp.Wait()
	if opts.JsonFormat {
		b, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			log.Fatalf("Error: %+v", err)
		}

		fmt.Printf("%s", b)
	}
}
