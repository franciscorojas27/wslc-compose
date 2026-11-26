package runtime

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	ansiReset   = "\033[0m"
	ansiBold    = "\033[1m"
	ansiDim     = "\033[90m"
	ansiRed     = "\033[31m"
	ansiGreen   = "\033[32m"
	ansiYellow  = "\033[33m"
	ansiBlue    = "\033[34m"
	ansiMagenta = "\033[35m"
	ansiCyan    = "\033[36m"
	ansiClear   = "\r\033[2K"
)

var (
	spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	brailleSteps  = []string{"⠀", "⡀", "⡄", "⡆", "⡇", "⣇", "⣧", "⣷", "⣿"}
	logColors     = []string{ansiCyan, ansiGreen, ansiYellow, ansiMagenta, ansiBlue}
)

type itemState int

const (
	statePending itemState = iota
	stateRunning
	stateDone
	stateFailed
)

type progressItem struct {
	key        string
	kind       string
	name       string
	action     string
	detail     string
	layers     []int
	percent    int
	hasPercent bool
	state      itemState
	started    time.Time
	elapsed    time.Duration
}

type progressUI struct {
	mu        sync.Mutex
	out       io.Writer
	isTTY     bool
	title     string
	items     []*progressItem
	index     map[string]*progressItem
	frame     int
	lastLines int
	stopCh    chan struct{}
	doneCh    chan struct{}
	once      sync.Once
}

func isStdoutTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func isStdinTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func newProgressUI(title string) *progressUI {
	return &progressUI{
		out:    os.Stdout,
		isTTY:  isStdoutTTY(),
		title:  title,
		index:  make(map[string]*progressItem),
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

func (u *progressUI) add(key, kind, name string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	item := &progressItem{
		key:    key,
		kind:   kind,
		name:   name,
		action: "Waiting",
		state:  statePending,
	}
	u.items = append(u.items, item)
	u.index[key] = item
}

func (u *progressUI) start() {
	u.mu.Lock()
	if u.isTTY {
		u.renderLocked()
		u.mu.Unlock()
		go u.loop()
		return
	}
	fmt.Fprintf(u.out, "[+] %s 0/%d\n", u.title, len(u.items))
	u.mu.Unlock()
}

func (u *progressUI) loop() {
	defer close(u.doneCh)
	ticker := time.NewTicker(120 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-u.stopCh:
			return
		case <-ticker.C:
			u.mu.Lock()
			u.frame++
			u.renderLocked()
			u.mu.Unlock()
		}
	}
}

func (u *progressUI) setRunning(key, action string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	item, ok := u.index[key]
	if !ok {
		return
	}
	if item.started.IsZero() {
		item.started = time.Now()
	}
	if action != "Pulling" {
		item.detail = ""
		item.layers = nil
		item.percent = 0
		item.hasPercent = false
	}
	item.action = action
	item.state = stateRunning

	if u.isTTY {
		u.renderLocked()
	}
}

func (u *progressUI) setProgress(key string, p PullProgress) {
	u.mu.Lock()
	defer u.mu.Unlock()

	item, ok := u.index[key]
	if !ok {
		return
	}
	if item.started.IsZero() {
		item.started = time.Now()
	}
	item.action = "Pulling"
	item.state = stateRunning
	if len(p.Layers) > 0 {
		item.layers = append([]int(nil), p.Layers...)
	}
	if p.HasPct {
		pct := p.Percent
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		if !item.hasPercent || pct >= item.percent {
			item.percent = pct
			item.hasPercent = true
		}
	}
	if p.Detail != "" {
		item.detail = p.Detail
	}

	if u.isTTY {
		u.renderLocked()
	}
}

func (u *progressUI) setDone(key, action string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	item, ok := u.index[key]
	if !ok {
		return
	}
	if !item.started.IsZero() {
		item.elapsed = time.Since(item.started)
	}
	item.detail = ""
	item.layers = nil
	item.percent = 0
	item.hasPercent = false
	item.action = action
	item.state = stateDone

	if u.isTTY {
		u.renderLocked()
		return
	}
	fmt.Fprintf(u.out, " ✔ %-9s %s %s %.1fs\n", item.kind, item.name, item.action, item.elapsed.Seconds())
}

func (u *progressUI) setFailed(key, action string) {
	u.mu.Lock()
	defer u.mu.Unlock()

	item, ok := u.index[key]
	if !ok {
		return
	}
	if !item.started.IsZero() {
		item.elapsed = time.Since(item.started)
	}
	item.detail = ""
	item.layers = nil
	item.percent = 0
	item.hasPercent = false
	item.action = action
	item.state = stateFailed

	if u.isTTY {
		u.renderLocked()
		return
	}
	fmt.Fprintf(u.out, " ✘ %-9s %s %s %.1fs\n", item.kind, item.name, item.action, item.elapsed.Seconds())
}

func (u *progressUI) finish() {
	u.once.Do(func() {
		if u.isTTY {
			close(u.stopCh)
			<-u.doneCh
			u.mu.Lock()
			u.renderLocked()
			u.mu.Unlock()
			return
		}
		u.mu.Lock()
		fmt.Fprintf(u.out, "[+] %s %d/%d\n", u.title, u.doneCountLocked(), len(u.items))
		u.mu.Unlock()
	})
}

func (u *progressUI) doneCountLocked() int {
	done := 0
	for _, item := range u.items {
		if item.state == stateDone {
			done++
		}
	}
	return done
}

func (u *progressUI) renderLocked() {
	var b strings.Builder

	if u.lastLines > 0 {
		fmt.Fprintf(&b, "\033[%dA", u.lastLines)
	}

	done := u.doneCountLocked()
	total := len(u.items)

	headerColor := ansiCyan
	if done == total && total > 0 {
		headerColor = ansiGreen
	}
	for _, item := range u.items {
		if item.state == stateFailed {
			headerColor = ansiRed
			break
		}
	}

	fmt.Fprintf(&b, "%s%s%s[+] %s %d/%d%s\n", ansiClear, ansiBold, headerColor, u.title, done, total, ansiReset)

	maxKind := 9
	maxName := 0
	maxAction := 10
	maxBraille := 0
	for _, item := range u.items {
		if len(item.kind) > maxKind {
			maxKind = len(item.kind)
		}
		if len(item.name) > maxName {
			maxName = len(item.name)
		}
		if len(item.action) > maxAction {
			maxAction = len(item.action)
		}
		if item.state == stateRunning && item.action == "Pulling" {
			w := len(item.layers) + 2
			if len(item.layers) == 0 {
				w = 8
			}
			if w > maxBraille {
				maxBraille = w
			}
		}
	}

	spinner := spinnerFrames[u.frame%len(spinnerFrames)]

	for _, item := range u.items {
		var icon string
		var actionColor string
		var durationStr string

		switch item.state {
		case statePending:
			icon = ansiDim + "•" + ansiReset
			actionColor = ansiDim
		case stateRunning:
			icon = ansiCyan + spinner + ansiReset
			actionColor = ansiCyan
			durationStr = fmt.Sprintf("%s%5.1fs%s", ansiDim, time.Since(item.started).Seconds(), ansiReset)
		case stateDone:
			icon = ansiGreen + "✔" + ansiReset
			actionColor = ansiGreen
			durationStr = fmt.Sprintf("%s%5.1fs%s", ansiDim, item.elapsed.Seconds(), ansiReset)
		case stateFailed:
			icon = ansiRed + "✘" + ansiReset
			actionColor = ansiRed
			durationStr = fmt.Sprintf("%s%5.1fs%s", ansiDim, item.elapsed.Seconds(), ansiReset)
		}

		if maxBraille > 0 {
			barCol := strings.Repeat(" ", maxBraille)
			if item.state == stateRunning && item.action == "Pulling" {
				barCol = formatBrailleColumn(item, u.frame, maxBraille)
			}
			fmt.Fprintf(
				&b,
				"%s %s %-*s  %-*s  %s  %s%-*s%s  %s\n",
				ansiClear,
				icon,
				maxKind, item.kind,
				maxName, item.name,
				barCol,
				actionColor, maxAction, item.action, ansiReset,
				durationStr,
			)
			continue
		}

		fmt.Fprintf(
			&b,
			"%s %s %-*s  %-*s  %s%-*s%s  %s\n",
			ansiClear,
			icon,
			maxKind, item.kind,
			maxName, item.name,
			actionColor, maxAction, item.action, ansiReset,
			durationStr,
		)
	}

	u.lastLines = total + 1
	fmt.Fprint(u.out, b.String())
}

func formatBrailleColumn(item *progressItem, frame int, width int) string {
	var b strings.Builder
	b.WriteString(ansiDim + "[" + ansiReset)

	if len(item.layers) > 0 {
		for _, lvl := range item.layers {
			if lvl < 0 {
				lvl = 0
			}
			if lvl >= len(brailleSteps) {
				lvl = len(brailleSteps) - 1
			}
			glyph := brailleSteps[lvl]
			switch {
			case lvl == 8:
				b.WriteString(ansiGreen + glyph + ansiReset)
			case lvl > 0:
				b.WriteString(ansiCyan + glyph + ansiReset)
			default:
				b.WriteString(ansiDim + glyph + ansiReset)
			}
		}
	} else {
		const cells = 6
		totalSteps := cells * 8
		pos := frame % (totalSteps + 4)
		if pos > totalSteps {
			pos = totalSteps
		}
		for i := 0; i < cells; i++ {
			cellVal := pos - i*8
			if cellVal < 0 {
				cellVal = 0
			}
			if cellVal > 8 {
				cellVal = 8
			}
			if cellVal > 0 {
				b.WriteString(ansiCyan + brailleSteps[cellVal] + ansiReset)
			} else {
				b.WriteString(ansiDim + brailleSteps[0] + ansiReset)
			}
		}
	}

	b.WriteString(ansiDim + "]" + ansiReset)
	content := b.String()
	if pad := width - visibleLen(content); pad > 0 {
		content += strings.Repeat(" ", pad)
	}
	return content
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

func visibleLen(s string) int {
	return utf8.RuneCountInString(stripANSI(s))
}

func printTable(out io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = visibleLen(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i >= len(widths) {
				break
			}
			if w := visibleLen(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	tty := isStdoutTTY()
	var b strings.Builder

	for i, h := range headers {
		if i > 0 {
			b.WriteString("   ")
		}
		if tty {
			b.WriteString(ansiBold)
			b.WriteString(h)
			b.WriteString(ansiReset)
		} else {
			b.WriteString(h)
		}
		if i < len(headers)-1 {
			if pad := widths[i] - visibleLen(h); pad > 0 {
				b.WriteString(strings.Repeat(" ", pad))
			}
		}
	}
	b.WriteByte('\n')

	for _, row := range rows {
		for i := range headers {
			if i > 0 {
				b.WriteString("   ")
			}
			cell := ""
			if i < len(row) {
				cell = row[i]
			}
			b.WriteString(cell)
			if i < len(headers)-1 {
				if pad := widths[i] - visibleLen(cell); pad > 0 {
					b.WriteString(strings.Repeat(" ", pad))
				}
			}
		}
		b.WriteByte('\n')
	}

	fmt.Fprint(out, b.String())
}
