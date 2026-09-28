// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package logs

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"
)

// rawLine is one physical line of a container log file, decoded.
type rawLine struct {
	ts      time.Time
	stream  string // stdout | stderr
	partial bool   // more of this logical line follows
	content []byte // without the trailing newline; aliases the read buffer
}

var errFormat = errors.New("not a CRI or docker json-file log line")

// parseLine decodes either container runtime format:
//
//	CRI (containerd, CRI-O):  <RFC3339Nano> <stdout|stderr> <P|F>[:tags] <content>
//	docker json-file:         {"log":"<content>\n","stream":"stdout","time":"<RFC3339Nano>"}
//
// In CRI a "P" tag marks a partial line (the runtime splits lines longer
// than its buffer, 16 KiB by default) that continues in the next "F"
// line. Docker marks partials by the absence of the trailing "\n" in
// "log". Either way the caller reassembles.
func parseLine(line []byte) (rawLine, error) {
	if len(line) > 0 && line[0] == '{' {
		return parseDocker(line)
	}
	return parseCRI(line)
}

func parseCRI(line []byte) (rawLine, error) {
	var r rawLine
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 <= 0 {
		return r, errFormat
	}
	ts, err := time.Parse(time.RFC3339Nano, string(line[:sp1]))
	if err != nil {
		return r, errFormat
	}
	rest := line[sp1+1:]
	sp2 := bytes.IndexByte(rest, ' ')
	if sp2 <= 0 {
		return r, errFormat
	}
	switch string(rest[:sp2]) {
	case "stdout":
		r.stream = "stdout"
	case "stderr":
		r.stream = "stderr"
	default:
		return r, errFormat
	}
	rest = rest[sp2+1:]
	// The tag field is followed by a space — unless the content is empty,
	// in which case some runtimes end the line right after the tag.
	sp3 := bytes.IndexByte(rest, ' ')
	tag := rest
	content := []byte{}
	if sp3 >= 0 {
		tag, content = rest[:sp3], rest[sp3+1:]
	}
	if i := bytes.IndexByte(tag, ':'); i >= 0 {
		tag = tag[:i] // future tags ride after a colon
	}
	switch string(tag) {
	case "P":
		r.partial = true
	case "F":
	default:
		return r, errFormat
	}
	r.ts, r.content = ts, content
	return r, nil
}

type dockerLine struct {
	Log    string `json:"log"`
	Stream string `json:"stream"`
	Time   string `json:"time"`
}

func parseDocker(line []byte) (rawLine, error) {
	var d dockerLine
	if err := json.Unmarshal(line, &d); err != nil {
		return rawLine{}, errFormat
	}
	if d.Stream != "stdout" && d.Stream != "stderr" {
		return rawLine{}, errFormat
	}
	ts, err := time.Parse(time.RFC3339Nano, d.Time)
	if err != nil {
		return rawLine{}, errFormat
	}
	r := rawLine{ts: ts, stream: d.Stream, content: []byte(d.Log)}
	if n := len(r.content); n > 0 && r.content[n-1] == '\n' {
		r.content = r.content[:n-1]
		if n := len(r.content); n > 0 && r.content[n-1] == '\r' {
			r.content = r.content[:n-1]
		}
	} else {
		r.partial = true
	}
	return r, nil
}
