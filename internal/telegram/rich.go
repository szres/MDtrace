package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
)

// Rich message limits from the Bot API "Rich Message Limits" section.
const (
	MaxRichBlocks       = 500
	MaxRichMedia        = 50
	MaxRichTableColumns = 20
)

// RichText is formatted text in a rich message: a string, a styled span, or a sequence of both.
type RichText struct{ value any }

func Text(s string) RichText { return RichText{value: s} }

func Bold(t RichText) RichText {
	return RichText{value: map[string]any{"type": "bold", "text": t.value}}
}

func Join(parts ...RichText) RichText {
	values := make([]any, len(parts))
	for i, p := range parts {
		values[i] = p.value
	}
	return RichText{value: values}
}

func (t RichText) MarshalJSON() ([]byte, error) {
	if t.value == nil {
		return json.Marshal("")
	}
	return json.Marshal(t.value)
}

// RichBlock is one block of an outgoing rich message (InputRichBlock).
type RichBlock interface {
	encode(files *attachments) (map[string]any, error)
}

type Heading struct {
	Text RichText
	Size int // 1 (largest) to 6
}

type Paragraph struct{ Text RichText }

type Footer struct{ Text RichText }

type TableCell struct {
	Text   RichText
	Header bool
	Align  string // left, center or right
}

type Table struct {
	Rows    [][]TableCell
	Striped bool
	Compact bool
}

// Media is a local photo, or an animation (GIF or silent MP4), uploaded with the message.
type Media struct {
	Path      string
	Caption   string
	Animation bool
}

// Slideshow shows its items one at a time with swipe navigation.
type Slideshow struct{ Items []Media }

func (b Heading) encode(*attachments) (map[string]any, error) {
	if b.Size < 1 || b.Size > 6 {
		return nil, fmt.Errorf("heading size %d out of range 1-6", b.Size)
	}
	return map[string]any{"type": "heading", "text": b.Text, "size": b.Size}, nil
}

func (b Paragraph) encode(*attachments) (map[string]any, error) {
	return map[string]any{"type": "paragraph", "text": b.Text}, nil
}

func (b Footer) encode(*attachments) (map[string]any, error) {
	return map[string]any{"type": "footer", "text": b.Text}, nil
}

func (b Table) encode(*attachments) (map[string]any, error) {
	cells := make([][]map[string]any, len(b.Rows))
	for i, row := range b.Rows {
		if len(row) > MaxRichTableColumns {
			return nil, fmt.Errorf("table row %d has %d columns, limit is %d", i, len(row), MaxRichTableColumns)
		}
		cells[i] = make([]map[string]any, len(row))
		for j, c := range row {
			align := c.Align
			if align == "" {
				align = "left"
			}
			cell := map[string]any{"text": c.Text, "align": align, "valign": "middle"}
			if c.Header {
				cell["is_header"] = true
			}
			cells[i][j] = cell
		}
	}
	block := map[string]any{"type": "table", "cells": cells}
	if b.Striped {
		block["is_striped"] = true
	}
	if b.Compact {
		block["is_compact"] = true
	}
	return block, nil
}

func (b Slideshow) encode(files *attachments) (map[string]any, error) {
	blocks := make([]map[string]any, 0, len(b.Items))
	for _, m := range b.Items {
		name, err := files.add(m.Path)
		if err != nil {
			return nil, err
		}
		kind := "photo"
		if m.Animation {
			kind = "animation"
		}
		block := map[string]any{
			"type": kind,
			kind:   map[string]any{"type": kind, "media": "attach://" + name},
		}
		if m.Caption != "" {
			block["caption"] = map[string]any{"text": Text(m.Caption)}
		}
		blocks = append(blocks, block)
	}
	return map[string]any{"type": "slideshow", "blocks": blocks}, nil
}

// attachments collects local files referenced as attach://<name> in the message.
type attachments struct {
	paths []string
	names []string
}

func (a *attachments) add(path string) (string, error) {
	if len(a.paths) >= MaxRichMedia {
		return "", fmt.Errorf("rich message exceeds %d media attachments", MaxRichMedia)
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("telegram: attachment: %w", err)
	}
	name := fmt.Sprintf("media%d", len(a.paths))
	a.paths = append(a.paths, path)
	a.names = append(a.names, name)
	return name, nil
}

// SendRich sends a rich message built from blocks; photos are uploaded in the same request.
func (c *Client) SendRich(ctx context.Context, blocks []RichBlock) error {
	if len(blocks) == 0 {
		return fmt.Errorf("telegram: rich message has no blocks")
	}
	files := &attachments{}
	encoded := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		m, err := b.encode(files)
		if err != nil {
			return fmt.Errorf("telegram: %w", err)
		}
		encoded = append(encoded, m)
	}
	if n := countBlocks(encoded); n > MaxRichBlocks {
		return fmt.Errorf("telegram: rich message has %d blocks, limit is %d", n, MaxRichBlocks)
	}
	message, err := json.Marshal(map[string]any{"blocks": encoded})
	if err != nil {
		return err
	}

	if len(files.paths) == 0 {
		payload, err := json.Marshal(map[string]any{"chat_id": c.chatID, "rich_message": json.RawMessage(message)})
		if err != nil {
			return err
		}
		return c.call(ctx, "sendRichMessage", bytes.NewReader(payload), "application/json")
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("chat_id", c.chatID); err != nil {
		return err
	}
	if err := form.WriteField("rich_message", string(message)); err != nil {
		return err
	}
	for i, path := range files.paths {
		if err := attachFile(form, files.names[i], path); err != nil {
			return err
		}
	}
	if err := form.Close(); err != nil {
		return err
	}
	return c.call(ctx, "sendRichMessage", &body, form.FormDataContentType())
}

// countBlocks counts blocks the way the limit does: nested blocks and table rows included.
func countBlocks(blocks []map[string]any) int {
	n := 0
	for _, b := range blocks {
		n++
		if nested, ok := b["blocks"].([]map[string]any); ok {
			n += countBlocks(nested)
		}
		if rows, ok := b["cells"].([][]map[string]any); ok {
			n += len(rows)
		}
	}
	return n
}

func attachFile(form *multipart.Writer, field, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("telegram: open attachment: %w", err)
	}
	defer f.Close()
	part, err := form.CreateFormFile(field, filepath.Base(path))
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, f); err != nil {
		return fmt.Errorf("telegram: read attachment %s: %w", filepath.Base(path), err)
	}
	return nil
}
