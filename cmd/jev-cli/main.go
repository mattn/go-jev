// Command jev-cli calls TypeSafe Jev from the shell.
//
//	echo 'Help! My payouts have been failing for 3 days.' |
//	  jev-cli choice 'Which team should handle this?' billing technical sales
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/mattn/go-jev"
)

const usage = `usage: jev-cli <command> [flags] args...

commands:
  noul   QUESTION                 probability (0..1) that the answer is yes
  choice QUESTION OPTION...       the chosen option; OPTION is name or name=description
  score  QUESTION LEVEL...        probability-weighted score across ordered levels
  grep   QUESTION                 print stdin lines whose answer is yes
  ask    QUESTIONS                answers for a JSON questions map (text or @file)
  version

The state (the text to judge) is read from stdin, or given with -s.
Run 'jev-cli <command> -h' for the flags of each command.

environment:
  TYPESAFE_API_KEY  API key (no Authorization header when unset)
  JEV_MODEL         model (default jev-latest)
  JEV_API_URL       endpoint; a bare host like localhost:8080 is expanded
  JEV_TIMEOUT       request timeout in seconds (default 60)

exit status: 0 ok / yes, 1 no (noul -q, grep without match), 2 error
`

// errNo is returned for a well-formed "no" (noul -q, grep without match).
var errNo = errors.New("no")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var cmd func(context.Context, *env, []string) error
	switch args[0] {
	case "noul":
		cmd = cmdNoul
	case "choice":
		cmd = cmdChoice
	case "score":
		cmd = cmdScore
	case "grep":
		cmd = cmdGrep
	case "ask":
		cmd = cmdAsk
	case "version", "-version", "--version":
		fmt.Fprintln(stdout, "jev-cli", jev.Version)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "jev-cli: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	e := &env{name: args[0], stdin: stdin, stdout: bufio.NewWriter(stdout), stderr: stderr}
	err := cmd(ctx, e, args[1:])
	e.stdout.Flush()
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errNo):
		return 1
	case errors.Is(err, flag.ErrHelp):
		return 0
	default:
		if msg := err.Error(); msg != "" {
			fmt.Fprintf(stderr, "jev-cli %s: %v\n", e.name, err)
		}
		return 2
	}
}

// env carries I/O and the flags shared by all commands.
type env struct {
	name   string
	stdin  io.Reader
	stdout *bufio.Writer
	stderr io.Writer

	fs        *flag.FlagSet
	state     *string
	jsonState bool
	lines     bool
	parallel  int
	jsonOut   bool
	model     string
	url       string
	timeout   time.Duration
	client    *jev.Client
}

func (e *env) flags(synopsis string) *flag.FlagSet {
	fs := flag.NewFlagSet("jev-cli "+e.name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "usage: jev-cli %s [flags] %s\n\nflags:\n", e.name, synopsis)
		fs.PrintDefaults()
	}
	fs.Func("s", "state `text` (default: read stdin)", func(s string) error {
		e.state = &s
		return nil
	})
	fs.BoolVar(&e.jsonState, "J", false, "state is JSON (with -l: one JSON value per line)")
	fs.BoolVar(&e.lines, "l", false, "treat each stdin line as a separate state; prints answer<TAB>line")
	fs.IntVar(&e.parallel, "P", 4, "concurrent requests with -l")
	fs.BoolVar(&e.jsonOut, "j", false, "print the answer as JSON")
	fs.StringVar(&e.model, "model", "", "model (default $JEV_MODEL or jev-latest)")
	fs.StringVar(&e.url, "url", "", "endpoint (default $JEV_API_URL or the TypeSafe API)")
	fs.DurationVar(&e.timeout, "timeout", 0, "request timeout (default $JEV_TIMEOUT or 60s)")
	e.fs = fs
	return fs
}

// parse parses flags anywhere among the positional arguments ("--" ends
// flags) and sets up the client.
func (e *env) parse(args []string) ([]string, error) {
	var pos []string
	for {
		if err := e.fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, errors.New("") // already reported by flag
		}
		rest := e.fs.Args()
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			pos = append(pos, rest...)
			break
		}
		if len(rest) == 0 {
			break
		}
		pos, args = append(pos, rest[0]), rest[1:]
	}

	e.client = jev.NewClient(e.clientOptions()...)
	if e.parallel < 1 {
		e.parallel = 1
	}
	if e.lines && e.state != nil {
		return nil, errors.New("-l and -s cannot be used together")
	}
	return pos, nil
}

// clientOptions builds the client configuration from the environment,
// overridden by flags.
func (e *env) clientOptions() []jev.ClientOption {
	opts := []jev.ClientOption{jev.WithAPIKey(os.Getenv("TYPESAFE_API_KEY"))}
	model, url := e.model, e.url
	if model == "" {
		model = os.Getenv("JEV_MODEL")
	}
	if model != "" {
		opts = append(opts, jev.WithModel(model))
	}
	if url == "" {
		url = os.Getenv("JEV_API_URL")
	}
	if url != "" {
		opts = append(opts, jev.WithURL(jev.Endpoint(url)))
	}
	timeout := e.timeout
	if timeout <= 0 {
		if n, err := strconv.Atoi(os.Getenv("JEV_TIMEOUT")); err == nil && n > 0 {
			timeout = time.Duration(n) * time.Second
		}
	}
	if timeout > 0 {
		opts = append(opts, jev.WithTimeout(timeout))
	}
	return opts
}

func (e *env) usageErr(format string, a ...any) error {
	fmt.Fprintf(e.stderr, "jev-cli %s: "+format+"\n", append([]any{e.name}, a...)...)
	e.fs.Usage()
	return errors.New("")
}

// stateValue converts input text into the value sent as "state".
func (e *env) stateValue(s string) (any, error) {
	if !e.jsonState {
		return s, nil
	}
	if !json.Valid([]byte(s)) {
		return nil, errors.New("state is not valid JSON")
	}
	return json.RawMessage(s), nil
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// result is what a command produced for one state.
type result struct {
	text string          // plain output
	json json.RawMessage // -j output
	noul float64         // noul probability, for -q / grep
	err  error
}

// each evaluates fn for the state(s) and calls emit with the results in input
// order. With -l, states are evaluated concurrently and a failed line is
// reported without stopping the others.
func (e *env) each(ctx context.Context, fn func(context.Context, any) result, emit func(line string, r result) error) error {
	if !e.lines {
		var text string
		if e.state != nil {
			text = *e.state
		} else {
			if isTerminal(e.stdin) {
				return errors.New("no state: pipe it via stdin or pass -s")
			}
			b, err := io.ReadAll(e.stdin)
			if err != nil {
				return err
			}
			text = strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
		}
		v, err := e.stateValue(text)
		if err != nil {
			return err
		}
		r := fn(ctx, v)
		if r.err != nil {
			return r.err
		}
		return emit(text, r)
	}

	type job struct {
		n    int
		line string
		done chan result
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan *job, e.parallel)
	sem := make(chan struct{}, e.parallel)
	var readErr error
	go func() {
		defer close(jobs)
		sc := bufio.NewScanner(e.stdin)
		sc.Buffer(make([]byte, 64*1024), 64*1024*1024)
		for n := 1; sc.Scan(); n++ {
			line := strings.TrimSuffix(sc.Text(), "\r")
			if strings.TrimSpace(line) == "" {
				continue
			}
			j := &job{n, line, make(chan result, 1)}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func() {
				defer func() { <-sem }()
				v, err := e.stateValue(j.line)
				if err != nil {
					j.done <- result{err: err}
					return
				}
				j.done <- fn(ctx, v)
			}()
			select {
			case jobs <- j:
			case <-ctx.Done():
				return
			}
		}
		readErr = sc.Err()
	}()

	failed := 0
	for j := range jobs {
		r := <-j.done
		if r.err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(e.stderr, "jev-cli %s: line %d: %v\n", e.name, j.n, r.err)
			failed++
			continue
		}
		if err := emit(j.line, r); err != nil {
			return err
		}
		// keep output flowing for long pipes
		if err := e.stdout.Flush(); err != nil {
			return err
		}
	}
	if readErr != nil {
		return readErr
	}
	if failed > 0 {
		return fmt.Errorf("%d line(s) failed", failed)
	}
	return nil
}

// print writes the result in the common output format.
func (e *env) print(line string, r result) error {
	out := r.text
	if e.jsonOut {
		out = string(r.json)
		if e.lines {
			state, _ := e.stateValue(line)
			b, err := json.Marshal(struct {
				State  any             `json:"state"`
				Answer json.RawMessage `json:"answer"`
			}{state, r.json})
			if err != nil {
				return err
			}
			out = string(b)
		}
	} else if e.lines {
		out += "\t" + line
	}
	_, err := fmt.Fprintln(e.stdout, out)
	return err
}

func compact(raw json.RawMessage) json.RawMessage {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return raw
	}
	return b.Bytes()
}

func ff(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// instructions sends text as-is, or as JSON with -I.
func instructions(s string, isJSON bool) (any, error) {
	if !isJSON {
		return s, nil
	}
	if !json.Valid([]byte(s)) {
		return nil, errors.New("instructions are not valid JSON")
	}
	return json.RawMessage(s), nil
}

// readArg returns s, or the contents of the file for "@file" ("@-" is stdin).
func readArg(s string, stdin io.Reader) (string, error) {
	if !strings.HasPrefix(s, "@") {
		return s, nil
	}
	var b []byte
	var err error
	if s == "@-" {
		b, err = io.ReadAll(stdin)
	} else {
		b, err = os.ReadFile(s[1:])
	}
	return string(b), err
}

func ask(e *env, q jev.Question) func(context.Context, any) result {
	return func(ctx context.Context, state any) result {
		a, err := e.client.Ask(ctx, state, q)
		if err != nil {
			return result{err: err}
		}
		if a.Type != "" && a.Type != q.Type {
			return result{err: fmt.Errorf("unexpected answer type %q", a.Type)}
		}
		return result{json: compact(a.Raw), noul: a.Noul, text: func() string {
			switch q.Type {
			case "noul":
				return ff(a.Noul)
			case "choice":
				return a.Choice
			default:
				return ff(a.Score)
			}
		}()}
	}
}

func cmdNoul(ctx context.Context, e *env, args []string) error {
	fs := e.flags("QUESTION")
	trueDesc := fs.String("true", "", "what a yes means")
	falseDesc := fs.String("false", "", "what a no means")
	quiet := fs.Bool("q", false, "print nothing; exit 0 if yes, 1 if no")
	threshold := fs.Float64("t", 0.5, "probability at or above which the answer counts as yes (-q)")
	isJSON := fs.Bool("I", false, "QUESTION is JSON (structured instructions)")
	pos, err := e.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return e.usageErr("need exactly one QUESTION")
	}
	if *quiet && e.lines {
		return e.usageErr("-q cannot be used with -l; see 'jev-cli grep'")
	}
	ins, err := instructions(pos[0], *isJSON)
	if err != nil {
		return err
	}
	q := jev.Question{Type: "noul", Instructions: ins}
	if *trueDesc != "" || *falseDesc != "" {
		q.Criteria = map[string]string{"true": *trueDesc, "false": *falseDesc}
	}
	var yes bool
	err = e.each(ctx, ask(e, q), func(line string, r result) error {
		yes = r.noul >= *threshold
		if *quiet {
			return nil
		}
		return e.print(line, r)
	})
	if err == nil && *quiet && !yes {
		return errNo
	}
	return err
}

func cmdGrep(ctx context.Context, e *env, args []string) error {
	fs := e.flags("QUESTION")
	trueDesc := fs.String("true", "", "what a yes means")
	falseDesc := fs.String("false", "", "what a no means")
	threshold := fs.Float64("t", 0.5, "probability at or above which a line matches")
	invert := fs.Bool("v", false, "print lines that do not match")
	withProb := fs.Bool("p", false, "prefix each printed line with its probability and a TAB")
	isJSON := fs.Bool("I", false, "QUESTION is JSON (structured instructions)")
	pos, err := e.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return e.usageErr("need exactly one QUESTION")
	}
	if e.state != nil {
		return e.usageErr("grep reads lines from stdin; -s is not supported")
	}
	e.lines = true
	ins, err := instructions(pos[0], *isJSON)
	if err != nil {
		return err
	}
	q := jev.Question{Type: "noul", Instructions: ins}
	if *trueDesc != "" || *falseDesc != "" {
		q.Criteria = map[string]string{"true": *trueDesc, "false": *falseDesc}
	}
	matched := false
	err = e.each(ctx, ask(e, q), func(line string, r result) error {
		if (r.noul >= *threshold) == *invert {
			return nil
		}
		matched = true
		if *withProb {
			line = r.text + "\t" + line
		}
		_, err := fmt.Fprintln(e.stdout, line)
		return err
	})
	if err == nil && !matched {
		return errNo
	}
	return err
}

// criteriaArg parses -c JSON. For choice, an array of names becomes an
// object with null descriptions, keeping order.
func criteriaArg(s string, choice bool) (any, error) {
	if !json.Valid([]byte(s)) {
		return nil, errors.New("-c is not valid JSON")
	}
	t := strings.TrimSpace(s)
	if choice && strings.HasPrefix(t, "[") {
		var names []any
		if err := json.Unmarshal([]byte(t), &names); err != nil {
			return nil, err
		}
		opts := make(jev.Options, len(names))
		for i, n := range names {
			name, ok := n.(string)
			if !ok {
				name = fmt.Sprint(n)
			}
			opts[i] = jev.Option{Name: name}
		}
		return opts, nil
	}
	return json.RawMessage(t), nil
}

func cmdChoice(ctx context.Context, e *env, args []string) error {
	fs := e.flags("QUESTION OPTION...")
	crit := fs.String("c", "", "options as JSON: {\"name\": \"description\"|null, ...} or [\"name\", ...] (text or @file)")
	isJSON := fs.Bool("I", false, "QUESTION is JSON (structured instructions)")
	pos, err := e.parse(args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return e.usageErr("need a QUESTION")
	}
	var criteria any
	if *crit != "" {
		if len(pos) > 1 {
			return e.usageErr("give options either as arguments or with -c, not both")
		}
		s, err := readArg(*crit, e.stdin)
		if err != nil {
			return err
		}
		if criteria, err = criteriaArg(s, true); err != nil {
			return err
		}
	} else {
		if len(pos) < 2 {
			return e.usageErr("need at least one OPTION")
		}
		opts := make(jev.Options, 0, len(pos)-1)
		for _, o := range pos[1:] {
			name, desc, ok := strings.Cut(o, "=")
			if ok {
				opts = append(opts, jev.Option{Name: name, Desc: desc})
			} else {
				opts = append(opts, jev.Option{Name: o})
			}
		}
		criteria = opts
	}
	ins, err := instructions(pos[0], *isJSON)
	if err != nil {
		return err
	}
	return e.each(ctx, ask(e, jev.Question{Type: "choice", Instructions: ins, Criteria: criteria}), e.print)
}

func cmdScore(ctx context.Context, e *env, args []string) error {
	fs := e.flags("QUESTION LEVEL...")
	crit := fs.String("c", "", "levels as a JSON array, lowest first (text or @file)")
	isJSON := fs.Bool("I", false, "QUESTION is JSON (structured instructions)")
	pos, err := e.parse(args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return e.usageErr("need a QUESTION")
	}
	var criteria any
	if *crit != "" {
		if len(pos) > 1 {
			return e.usageErr("give levels either as arguments or with -c, not both")
		}
		s, err := readArg(*crit, e.stdin)
		if err != nil {
			return err
		}
		if criteria, err = criteriaArg(s, false); err != nil {
			return err
		}
	} else {
		if len(pos) < 3 {
			return e.usageErr("need at least two LEVELs, lowest first")
		}
		criteria = pos[1:]
	}
	ins, err := instructions(pos[0], *isJSON)
	if err != nil {
		return err
	}
	return e.each(ctx, ask(e, jev.Question{Type: "score", Instructions: ins, Criteria: criteria}), e.print)
}

func cmdAsk(ctx context.Context, e *env, args []string) error {
	fs := e.flags("QUESTIONS")
	raw := fs.Bool("raw", false, "print the whole response (model, answers, usage)")
	pos, err := e.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return e.usageErr("need exactly one QUESTIONS (JSON text or @file)")
	}
	s, err := readArg(pos[0], e.stdin)
	if err != nil {
		return err
	}
	s = strings.TrimSpace(s)
	if !json.Valid([]byte(s)) || !strings.HasPrefix(s, "{") {
		return errors.New("QUESTIONS must be a JSON object of questions")
	}
	e.jsonOut = true // answers are always JSON
	return e.each(ctx, func(ctx context.Context, state any) result {
		resp, err := e.client.Evaluate(ctx, state, json.RawMessage(s))
		if err != nil {
			return result{err: err}
		}
		if *raw {
			return result{json: compact(resp.Raw)}
		}
		var body struct {
			Answers json.RawMessage `json:"answers"`
		}
		if err := json.Unmarshal(resp.Raw, &body); err != nil || body.Answers == nil {
			return result{err: errors.New("unexpected response: no answers")}
		}
		return result{json: compact(body.Answers)}
	}, e.print)
}
