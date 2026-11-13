//go:build !raw && !android

package main

import (
	"fmt"
	"io"
	"os"

	findfont "github.com/flopp/go-findfont"
	"github.com/sqweek/dialog"
)

// Log writer implementation
func NewLogWriter() io.Writer {
	return os.Stderr
}

// Message box implementation
func ShowInfoDialog(message, title string) {
	if ovOn || hostPerfOn { // Hidden tools and overlay: never a window on the desktop
		fmt.Fprintln(os.Stderr, title+": "+message)
		return
	}
	dialog.Message(message).Title(title).Info()
}

func ShowErrorDialog(message string) {
	if ovOn || hostPerfOn {
		fmt.Fprintln(os.Stderr, "I.K.E.M.E.N Error: "+message)
		return
	}
	dialog.Message(message).Title("I.K.E.M.E.N Error").Error()
}

// TTF font loading
func LoadFntTtf(f *Fnt, fontfile string, filename string, height int32) {
	// Host overlay: no text is ever drawn (fight screen and motif are off) and TrueType glyph atlases are
	// GL work on the loading thread - the font stays empty (TextWidth reads 0)
	if ovOn {
		if height != -1 {
			f.Size[1] = uint16(height)
		}
		f.palettes = make([][256]uint32, 1)
		return
	}
	// Search in local directory
	fileDir := SearchFile(filename, []string{fontfile, sys.motif.Def, "", "data/"}, "font/")
	// Search in system directory
	fp := fileDir
	if fp = FileExist(fp); len(fp) == 0 {
		var err error
		fileDir, err = findfont.Find(fileDir)
		if err != nil {
			panic(fmt.Errorf("failed to find ttf font %v: %w", fileDir, err))
		}
	}
	// Load ttf
	if height == -1 {
		height = int32(f.Size[1])
	} else {
		f.Size[1] = uint16(height)
	}
	ttf, err := gfxFont.LoadFont(fileDir, height, int(sys.gameWidth), int(sys.gameHeight))
	if err != nil {
		panic(fmt.Errorf("failed to load ttf font %v: %w", fileDir, err))
	}
	f.ttf = ttf.(Font)

	// Create Ttf dummy palettes
	f.palettes = make([][256]uint32, 1)
	for i := 0; i < 256; i++ {
		f.palettes[0][i] = 0
	}
}

func selectRenderer(cfgVal string) (Renderer, FontRenderer) {
	var gfx Renderer
	var gfxFont FontRenderer

	// Now we proceed to init the render.
	switch cfgVal {
	case "OpenGL 3.3":
		gfx = &Renderer_GL33{}
		gfxFont = &FontRenderer_GL33{}
	case "Vulkan 1.3":
		gfx = &Renderer_VK{}
		gfxFont = &FontRenderer_VK{}
	default:
		fmt.Printf("Error: Invalid RenderMode '%s'. Defaulting to OpenGL 3.3.\n", cfgVal)
		gfx = &Renderer_GL33{}
		gfxFont = &FontRenderer_GL33{}
	}

	return gfx, gfxFont
}

func Logcat(s string) {
	fmt.Println(s)
}
