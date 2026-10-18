package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Foreground colors are ANSI palette indices so the terminal's own theme always
// applies. Background tints can't be expressed that way, so they are blended
// from the Ghostty theme's actual colors when available.
var (
	cAccent  lipgloss.TerminalColor = lipgloss.Color("4")
	cBorder  lipgloss.TerminalColor = lipgloss.Color("8")
	cDim     lipgloss.TerminalColor = lipgloss.Color("8")
	cText    lipgloss.TerminalColor = lipgloss.NoColor{}
	cGreen   lipgloss.TerminalColor = lipgloss.Color("2")
	cRed     lipgloss.TerminalColor = lipgloss.Color("1")
	cYellow  lipgloss.TerminalColor = lipgloss.Color("3")
	cMagenta lipgloss.TerminalColor = lipgloss.Color("5")
	cCyan    lipgloss.TerminalColor = lipgloss.Color("6")
	cBlack   lipgloss.TerminalColor = lipgloss.Color("0")

	cSelBg  lipgloss.TerminalColor = lipgloss.Color("#2f3a5c")
	cSelDim lipgloss.TerminalColor = lipgloss.Color("#232a3d")
	cPopBg  lipgloss.TerminalColor = lipgloss.Color("#1f2335")
	cHlBg   lipgloss.TerminalColor = lipgloss.Color("#3d2f5c")
	cDelBg  lipgloss.TerminalColor = lipgloss.Color("#3a2028")
	cAddBg  lipgloss.TerminalColor = lipgloss.Color("#1f3527")
	cFileBg lipgloss.TerminalColor = lipgloss.Color("#2a2f45")

	lanePalette = []lipgloss.TerminalColor{
		lipgloss.Color("4"), lipgloss.Color("2"), lipgloss.Color("5"), lipgloss.Color("3"),
		lipgloss.Color("1"), lipgloss.Color("6"), lipgloss.Color("12"), lipgloss.Color("10"),
		lipgloss.Color("13"), lipgloss.Color("11"), lipgloss.Color("9"), lipgloss.Color("14"),
	}

	sPlain = lipgloss.NewStyle()
	sDim   = lipgloss.NewStyle().Foreground(cDim)
)

type ghosttyColors struct {
	palette    [16]string
	background string
	foreground string
	selection  string
}

func applyTheme() {
	gc, ok := loadGhosttyColors()
	if !ok {
		return
	}
	bg, fg := gc.background, gc.foreground
	pal := func(i int, fallback string) string {
		if gc.palette[i] != "" {
			return gc.palette[i]
		}
		return fallback
	}
	if bg != "" {
		cBlack = lipgloss.Color(bg) // text drawn on colored badges
	}
	if bg == "" || fg == "" {
		return
	}
	sel := gc.selection
	if sel == "" {
		sel = blend(bg, fg, 0.20)
	}
	cSelBg = lipgloss.Color(sel)
	cSelDim = lipgloss.Color(blend(bg, fg, 0.08))
	cPopBg = lipgloss.Color(blend(bg, pal(0, "#000000"), 0.5))
	cHlBg = lipgloss.Color(blend(bg, pal(5, "#ff00ff"), 0.30))
	cDelBg = lipgloss.Color(blend(bg, pal(1, "#ff0000"), 0.18))
	cAddBg = lipgloss.Color(blend(bg, pal(2, "#00ff00"), 0.15))
	cFileBg = lipgloss.Color(blend(bg, fg, 0.12))
}

// loadGhosttyColors reads Ghostty's config files (and the theme they name) the
// same way Ghostty does: theme first, explicit color keys override it.
func loadGhosttyColors() (ghosttyColors, bool) {
	home, _ := os.UserHomeDir()
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(home, ".config")
	}
	files := []string{
		filepath.Join(xdg, "ghostty", "config"),
		filepath.Join(home, "Library", "Application Support", "com.mitchellh.ghostty", "config"),
	}
	var theme string
	var overrides [][2]string
	seen := map[string]bool{}
	var read func(path string, depth int)
	read = func(path string, depth int) {
		if depth > 4 || seen[path] {
			return
		}
		seen[path] = true
		for _, kv := range readKV(path) {
			switch kv[0] {
			case "theme":
				theme = kv[1]
			case "palette", "background", "foreground", "selection-background":
				overrides = append(overrides, kv)
			case "config-file":
				p := strings.TrimPrefix(kv[1], "?")
				if !filepath.IsAbs(p) {
					p = filepath.Join(filepath.Dir(path), p)
				}
				read(p, depth+1)
			}
		}
	}
	for _, f := range files {
		read(f, 0)
	}
	if len(seen) == 0 || (theme == "" && len(overrides) == 0) {
		return ghosttyColors{}, false
	}

	var gc ghosttyColors
	if theme != "" {
		if path := findTheme(pickTheme(theme), xdg); path != "" {
			for _, kv := range readKV(path) {
				gc.set(kv[0], kv[1])
			}
		}
	}
	for _, kv := range overrides {
		gc.set(kv[0], kv[1])
	}
	return gc, gc.background != "" || gc.palette[1] != ""
}

func (gc *ghosttyColors) set(key, val string) {
	switch key {
	case "palette":
		idx, color, ok := strings.Cut(val, "=")
		if n, err := strconv.Atoi(strings.TrimSpace(idx)); ok && err == nil && n >= 0 && n < 16 {
			gc.palette[n] = normHex(color)
		}
	case "background":
		gc.background = normHex(val)
	case "foreground":
		gc.foreground = normHex(val)
	case "selection-background":
		gc.selection = normHex(val)
	}
}

// pickTheme resolves "light:A,dark:B" using the macOS appearance.
func pickTheme(v string) string {
	if !strings.Contains(v, ":") {
		return v
	}
	want := "light"
	if out, err := exec.Command("defaults", "read", "-g", "AppleInterfaceStyle").Output(); err == nil &&
		strings.TrimSpace(string(out)) == "Dark" {
		want = "dark"
	}
	for _, part := range strings.Split(v, ",") {
		if k, name, ok := strings.Cut(strings.TrimSpace(part), ":"); ok && k == want {
			return name
		}
	}
	return v
}

func findTheme(name, xdg string) string {
	if filepath.IsAbs(name) {
		return name
	}
	dirs := []string{filepath.Join(xdg, "ghostty", "themes")}
	if r := os.Getenv("GHOSTTY_RESOURCES_DIR"); r != "" {
		dirs = append(dirs, filepath.Join(r, "themes"))
	}
	dirs = append(dirs, "/Applications/Ghostty.app/Contents/Resources/ghostty/themes")
	for _, d := range dirs {
		if p := filepath.Join(d, name); exists(p) {
			return p
		}
	}
	return ""
}

func readKV(path string) [][2]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][2]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out = append(out, [2]string{strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"`)})
	}
	return out
}

// normHex accepts "#rrggbb" or "rrggbb"; named colors are not supported.
func normHex(s string) string {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	if len(s) != 6 {
		return ""
	}
	if _, err := strconv.ParseUint(s, 16, 32); err != nil {
		return ""
	}
	return "#" + strings.ToLower(s)
}

func blend(a, b string, t float64) string {
	pa, pb := parseHex(a), parseHex(b)
	var c [3]int
	for i := range c {
		c[i] = int(float64(pa[i])*(1-t) + float64(pb[i])*t + 0.5)
	}
	return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2])
}

func parseHex(s string) [3]int {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	return [3]int{int(v >> 16 & 0xff), int(v >> 8 & 0xff), int(v & 0xff)}
}
