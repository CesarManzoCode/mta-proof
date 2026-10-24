package dst

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/CesarManzoCode/mta-proof/internal/classify"
)

// CorpusCase is one case of corpus/classification.json (contract 3.8).
type CorpusCase struct {
	ID              string `json:"id"`
	Provider        string `json:"provider"`
	Code            int    `json:"code"`
	Enhanced        string `json:"enhanced"`
	Text            string `json:"text"`
	ExpectedClass   string `json:"expected_class"`
	ExpectedSubtype string `json:"expected_subtype"`
	Source          string `json:"source"`
}

// Decision is one line of the classify output: what the engine's
// classifier decided for a corpus case. The expected values are not
// copied, so whoever compares reads them from the corpus.
type Decision struct {
	ID      string `json:"id"`
	Class   string `json:"class"`
	Subtype string `json:"subtype"`
	RuleID  string `json:"rule_id"`
}

// ClassifyCorpus classifies every case of the corpus at corpusPath with
// the engine's rule table and writes one Decision per line to outPath, in
// corpus order.
func ClassifyCorpus(corpusPath, outPath string) (int, error) {
	data, err := os.ReadFile(corpusPath)
	if err != nil {
		return 0, err
	}
	var cases []CorpusCase
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cases); err != nil {
		return 0, fmt.Errorf("%s: %w", corpusPath, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return 0, fmt.Errorf("%s: trailing data after the corpus", corpusPath)
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if c.ID == "" || seen[c.ID] {
			return 0, fmt.Errorf("%s: case id %q is empty or repeated", corpusPath, c.ID)
		}
		seen[c.ID] = true
	}
	if len(cases) == 0 {
		return 0, errors.New("empty corpus")
	}
	f, err := os.Create(outPath)
	if err != nil {
		return 0, err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, c := range cases {
		d := classify.Classify(classify.Reply{Provider: c.Provider, Code: c.Code, Enhanced: c.Enhanced, Text: c.Text})
		if err := enc.Encode(Decision{ID: c.ID, Class: d.Class, Subtype: d.Subtype, RuleID: d.RuleID}); err != nil {
			f.Close()
			return 0, err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return 0, err
	}
	return len(cases), f.Close()
}
