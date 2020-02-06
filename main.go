package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/json"
	"flag"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/artyom/autoflags"
	yaml "gopkg.in/yaml.v2"
)

func main() {
	config := struct {
		Addr     string        `flag:"listen,address to listen at"`
		Qsize    int           `flag:"qsize,job queue size"`
		Config   string        `flag:"config,path to config (yaml)"`
		CertFile string        `flag:"cert,path to ssl certificate"`
		KeyFile  string        `flag:"key,path to ssl certificate key"`
		Timeout  time.Duration `flag:"timeout,timeout for command run"`
		Verbose  bool          `flag:"verbose,pass stdout/stderr from commands to stderr"`
	}{
		Addr:    "127.0.0.1:8080",
		Qsize:   10,
		Timeout: 3 * time.Minute,
	}
	autoflags.Define(&config)
	flag.Parse()
	cfg, err := readConfig(config.Config)
	if err != nil {
		log.Fatal(err)
	}
	if config.Qsize < 1 {
		config.Qsize = 1
	}
	h := hookHandler{
		cmds:    make(chan execEnv, config.Qsize),
		timeout: config.Timeout,
		verbose: config.Verbose,
	}
	for k, v := range cfg {
		http.HandleFunc(k, h.endpointHandler(v))
	}
	go h.run()
	server := &http.Server{
		Addr:           config.Addr,
		MaxHeaderBytes: 1 << 20,
		ReadTimeout:    15 * time.Second,
		WriteTimeout:   15 * time.Second,
	}
	if len(config.CertFile) > 0 && len(config.KeyFile) > 0 {
		log.Fatal(server.ListenAndServeTLS(config.CertFile, config.KeyFile))
	}
	log.Fatal(server.ListenAndServe())
}

// hookHandler manages receiving/dispatching hook requests and running
// corresponding commands
type hookHandler struct {
	cmds    chan execEnv
	timeout time.Duration
	verbose bool
}

// run receives commands to run on channel and executes them
func (hh hookHandler) run() {
	cmdRun := func(item execEnv) error {
		ctx := context.Background()
		if hh.timeout > 0 {
			var cancel func()
			ctx, cancel = context.WithTimeout(ctx, hh.timeout)
			defer cancel()
		}
		var cmd *exec.Cmd
		c, ok := item.endpoint.Refs[item.payload.Ref]
		switch {
		case ok:
			log.Print("found per-ref command")
			cmd = exec.CommandContext(ctx, c.Command, c.Args...)
		case !ok && len(item.endpoint.Command) > 0:
			log.Print("found global per-repo command")
			cmd = exec.CommandContext(ctx,
				item.endpoint.Command,
				item.endpoint.Args...)
		default:
			log.Printf("no matching command for ref %q found, skipping",
				item.payload.Ref)
			return nil
		}
		log.Printf("repo: %q, ref: %q, command: %v",
			item.endpoint.RepoName, item.payload.Ref, cmd.Args)
		if hh.verbose {
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
		}
		return cmd.Run()
	}
	for item := range hh.cmds {
		if err := cmdRun(item); err != nil {
			log.Printf("repo: %q, ref: %q, command run: %v",
				item.endpoint.RepoName, item.payload.Ref, err)
		}
	}
}

// endpointHandler constructs http.HandlerFunc for particular endpoint
func (hh hookHandler) endpointHandler(ep endpoint) http.HandlerFunc {
	secret := []byte(ep.Secret)
	withSecret := len(ep.Secret) > 0
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "unsupported method",
				http.StatusMethodNotAllowed)
			return
		}
		switch r.Header.Get("X-Github-Event") {
		case "push":
		case "ping":
			return // accept with code 200
		default:
			http.Error(w, "unsupported event type",
				http.StatusBadRequest)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "unsupported content type",
				http.StatusUnsupportedMediaType)
			return
		}
		var sig string
		if n, err := fmt.Sscanf(
			r.Header.Get("X-Hub-Signature"),
			"sha1=%s", &sig); n != 1 || err != nil {
			http.Error(w, "malformed signature", http.StatusForbidden)
			return
		}
		var (
			tr      io.Reader = r.Body
			mac     hash.Hash
			payload pushPayload
		)
		if withSecret {
			mac = hmac.New(sha1.New, secret)
			tr = io.TeeReader(r.Body, mac)
		}
		if err := json.NewDecoder(tr).Decode(&payload); err != nil {
			log.Print(err)
			http.Error(w, "malformed json",
				http.StatusInternalServerError)
			return
		}
		if withSecret {
			sig2 := fmt.Sprintf("%x", mac.Sum(nil))
			if sig != sig2 {
				log.Printf("signature mismatch, got %q, want %q", sig, sig2)
				http.Error(w, "signature mismatch",
					http.StatusPreconditionFailed)
				return
			}
		}
		if payload.Repository.Name != ep.RepoName {
			log.Printf("repository names mismatch: got %q, want %q",
				payload.Repository.Name, ep.RepoName)
			http.Error(w, "repository mismatch",
				http.StatusPreconditionFailed)
			return
		}
		select {
		case hh.cmds <- execEnv{payload, ep}:
		default: // spillover
			log.Print("buffer spillover")
			http.Error(w, "spillover", http.StatusServiceUnavailable)
			return
		}
	}
}

// execEnv used to pass both payload and endpoint info via channel
type execEnv struct {
	payload  pushPayload
	endpoint endpoint
}

type pushPayload struct {
	Ref        string `json:"ref"`
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		HttpUrl  string `json:"html_url"`
		SshUrl   string `json:"ssh_url"`
		GitUrl   string `json:"git_url"`
		CloneUrl string `json:"clone_url"`
	} `json:"repository"`
}

// endpoint represents config for one repository, handled by particular url
type endpoint struct {
	RepoName string
	Secret   string
	Command  string // global command used if no per-ref command found
	Args     []string
	Refs     map[string]struct {
		Command string // per-ref commands
		Args    []string
	}
}

// readConfig loads configuration from yaml file
//
// Config should be in form map[string]endpoint, where keys are urls used to set
// up http handlers.
func readConfig(fileName string) (map[string]endpoint, error) {
	b, err := ioutil.ReadFile(fileName)
	if err != nil {
		return nil, err
	}
	out := make(map[string]endpoint)
	if err := yaml.Unmarshal(b, out); err != nil {
		return nil, err
	}
	return out, nil
}
