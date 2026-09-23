# go-jev

Go SDK for [TypeSafe Jev](https://docs.typesafe.ai/), a decision-only model
that returns typed answers (yes/no probability, choice, score) instead of
text. Comes with `jev`, a command-line tool built on it for UNIX pipelines.

## Install

```sh
go get github.com/mattn/go-jev
```

## Usage

```go
import "github.com/mattn/go-jev"

c := jev.NewClient(jev.WithAPIKey(os.Getenv("TYPESAFE_API_KEY")))

// one question
a, err := c.Ask(ctx, "Help! My payouts have been failing for 3 days.", jev.Question{
	Type:         "choice",
	Instructions: "Which team should handle this?",
	Criteria: jev.Options{ // keeps option order
		{Name: "billing", Desc: "Payments, invoicing, refunds"},
		{Name: "technical", Desc: "Bugs, outages, integrations"},
		{Name: "sales"}, // nil Desc is sent as null
	},
})
if err != nil {
	log.Fatal(err)
}
fmt.Println(a.Choice, a.Confidence, a.Probabilities)

// several questions in one request; state may be any JSON value
resp, err := c.Evaluate(ctx, map[string]any{"from": "alice", "text": "refund me"},
	map[string]jev.Question{
		"urgent":      {Type: "noul", Instructions: "Is this urgent?"},
		"frustration": {Type: "score", Instructions: "How frustrated?", Criteria: []string{"Calm", "Frustrated", "Very angry"}},
	})
if err != nil {
	log.Fatal(err)
}
fmt.Println(resp.Answers["urgent"].Noul, resp.Answers["frustration"].Score)
```

| | |
|---|---|
| `NewClient(opts...)` | client for the TypeSafe API; the package reads no environment variables |
| `WithAPIKey(key)` | API key; without one no `Authorization` header is sent |
| `WithModel(name)` | model (default `jev-latest`) |
| `WithURL(url)` | endpoint, as given (default `https://api.typesafe.ai/v1/systemone`) |
| `WithTimeout(d)` | limit per request including retries (default 60s, 0 = none) |
| `WithHTTPClient(hc)`, `WithMaxRetries(n)` | HTTP client, retries on 429 / 529 (default 3) |
| `Client.Ask(ctx, state, q)` | one `Question` → `*Answer` |
| `Client.Evaluate(ctx, state, questions)` | a questions map → `*Response` (`Answers`, `Usage`, `Raw`) |
| `Question` | `Type` (`"noul"`, `"choice"`, `"score"`), `Instructions`, `Criteria` |
| `Options` | ordered choice criteria |
| `Answer` | `Noul`, `Choice`, `Score`, `Legend`, `Probabilities`, `Confidence`, `Raw` |
| `*APIError` | non-2xx response (`Status`, `Body`) |
| `Endpoint(addr)` | expands `localhost:8080` to `http://localhost:8080/v1/systemone` |

`state`, `Instructions` and `Criteria` take any value that marshals to JSON;
a `json.RawMessage` is sent verbatim. `429` and `529` are retried with
exponential backoff.

A local [tensai](https://github.com/mattn/tensai) server speaks the same API:

```go
c := jev.NewClient(jev.WithURL(jev.Endpoint("localhost:8080")))
```

## Command-line tool

```sh
go install github.com/mattn/go-jev/cmd/jev@latest
export TYPESAFE_API_KEY=...
```

```sh
# yes/no → probability 0..1
echo 'Help! My payouts have been failing for 3 days.' | jev noul 'Does this convey urgency?'
# 0.95

# pick one option (name or name=description) → chosen option
echo 'Help! My payouts have been failing for 3 days.' |
  jev choice 'Which team should handle this?' \
    billing='Payments, invoicing, refunds' technical='Bugs, outages, integrations' sales
# billing

# rate on ordered levels (lowest first) → probability-weighted score
jev score -s 'This is the third time!' 'How frustrated is the customer?' Calm Frustrated 'Very angry'
# 1.05

# yes/no as exit status
if jev noul -q 'Is this spam?' < mail.txt; then mv mail.txt spam/; fi

# one state per line → answer<TAB>line, in input order, 4 requests at a time
jev choice -l 'いま何を飲む？' コーヒー ビール 紅茶 < scenes.txt
# ビール	金曜の夜、友人と居酒屋
# コーヒー	月曜の朝、出社前

# grep by meaning (-v invert, -p prefix probability, -t threshold)
jev grep 'Is the customer complaining?' < reviews.txt

# several questions in one request → answers JSON
jev ask '{"urgent":{"type":"noul","instructions":"Urgent?"},
          "team":{"type":"choice","instructions":"Team?","criteria":{"billing":null,"technical":null}}}' < ticket.txt
jev ask @questions.json < ticket.txt | jq .team.choice
```

### Commands

| Command | Prints |
|---|---|
| `jev noul QUESTION` | probability that the answer is yes (`-true`/`-false` describe each side, `-q` exit status only, `-t` threshold) |
| `jev choice QUESTION OPTION...` | the chosen option (`-c` takes a JSON object or array instead) |
| `jev score QUESTION LEVEL...` | the weighted score (`-c` takes a JSON array instead) |
| `jev grep QUESTION` | stdin lines whose answer is yes |
| `jev ask QUESTIONS` | the `answers` object for a [questions map](https://docs.typesafe.ai/api.md) (`-raw` for the whole response) |

Common flags (can appear anywhere; `--` ends flags):

| Flag | |
|---|---|
| `-s TEXT` | state text instead of stdin |
| `-J` | state is JSON (chat logs, records, ...); with `-l`, JSON lines |
| `-I` | QUESTION is JSON (structured instructions) |
| `-l` | each stdin line is a separate state; prints `answer<TAB>line` |
| `-P N` | concurrent requests with `-l` (default 4) |
| `-j` | print the answer JSON (with `-l`: `{"state":...,"answer":...}` per line) |
| `-model`, `-url`, `-timeout` | override the environment below |

Exit status: `0` ok / yes, `1` no (`noul -q`, `grep` without a match), `2` error.
With `-l`, a failed line is reported on stderr and the rest continue.
`429` and `529` are retried with exponential backoff (up to 3 times).

### Configuration

| Environment variable | Default |
|---|---|
| `TYPESAFE_API_KEY` | (none; no `Authorization` header is sent) |
| `JEV_MODEL` | `jev-latest` |
| `JEV_API_URL` | `https://api.typesafe.ai/v1/systemone` |
| `JEV_TIMEOUT` | `60` (seconds) |

The `jev` command reads these and passes them to the client; flags
(`-model`, `-url`, `-timeout`) override them. They match
[sqlite3-jev](https://github.com/mattn/sqlite3-jev). A bare host
such as `localhost:8080` expands to `http://localhost:8080/v1/systemone`, so a
local [tensai](https://github.com/mattn/tensai) server works as is:

```sh
JEV_API_URL=localhost:8080 jev choice -s '金曜の夜、友人と居酒屋' 'いま何を飲む？' コーヒー ビール 紅茶
```

## License

MIT
