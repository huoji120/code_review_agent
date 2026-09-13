package forum

import (
	"strings"
	"unicode/utf8"
)

// Search is literal, not regex or semantic similarity. All terms must occur in
// one retained message (its title and body may each supply a different term).
type postSearch struct {
	terms []string
	all   bool
}

func newPostSearch(query, mode string) postSearch {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return postSearch{}
	}
	if mode == "all" || mode == "any" {
		return postSearch{terms: strings.Fields(query), all: mode == "all"}
	}
	return postSearch{terms: []string{query}, all: true}
}

func (q postSearch) matches(m Message) bool {
	if len(q.terms) == 0 {
		return true
	}
	topic, body := strings.ToLower(m.Topic), strings.ToLower(m.Content)
	for _, term := range q.terms {
		found := strings.Contains(topic, term) || strings.Contains(body, term)
		if found && !q.all {
			return true
		}
		if !found && q.all {
			return false
		}
	}
	return q.all
}

func (q postSearch) message(p Post) Message {
	if !p.RootMissing && q.matches(p.Root) {
		return p.Root
	}
	for _, m := range p.Replies {
		if q.matches(m) {
			return m
		}
	}
	return Message{}
}

type searchHit struct {
	Term      string `json:"term"`
	Field     string `json:"field"`
	Offset    int    `json:"offset"`
	EndOffset int    `json:"end_offset"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
}

type searchReadArgs struct {
	MessageID int64 `json:"message_id"`
	Offset    int   `json:"offset"`
	All       bool  `json:"all"`
}

// Lowercasing can change UTF-8 byte lengths (e.g. K -> k); convert rune
// positions back to the original text before exposing offsets to forum_read.
func originalSpan(text, lower string, index, length int) (int, int) {
	before := utf8.RuneCountInString(lower[:index])
	count := utf8.RuneCountInString(lower[index : index+length])
	start, end, runeIndex := len(text), len(text), 0
	for offset := range text {
		if runeIndex == before {
			start = offset
		}
		if runeIndex == before+count {
			end = offset
			break
		}
		runeIndex++
	}
	return start, end
}

func (q postSearch) hits(m Message) []searchHit {
	var hits []searchHit
	body, topic := strings.ToLower(m.Content), strings.ToLower(m.Topic)
	for _, term := range q.terms {
		field, text, lower := "content", m.Content, body
		index := strings.Index(lower, term)
		if index < 0 {
			field, text, lower = "topic", m.Topic, topic
			index = strings.Index(lower, term)
		}
		if index < 0 {
			continue
		}
		start, end := originalSpan(text, lower, index, len(term))
		lineStart := strings.LastIndexByte(text[:start], '\n') + 1
		hits = append(hits, searchHit{Term: term, Field: field, Offset: start, EndOffset: end, Line: strings.Count(text[:start], "\n") + 1, Column: utf8.RuneCountInString(text[lineStart:start]) + 1})
	}
	return hits
}

func searchExcerpt(text, field string, hits []searchHit, limit int) (string, int) {
	offset := 0
	for _, hit := range hits {
		if hit.Field != field {
			continue
		}
		offset = hit.Offset
		// Include nearby preceding evidence, without breaking a UTF-8 character.
		for n := 0; offset > 0 && n < 16; n++ {
			_, size := utf8.DecodeLastRuneInString(text[:offset])
			offset -= size
		}
		break
	}
	return excerpt(text[offset:], limit), offset
}
