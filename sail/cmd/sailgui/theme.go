package main

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// mono is the Sailnet look: midnight, mint and foam, flat. No rounded
// corners, no shadows. (brand/README.md)
type mono struct{}

var (
	black = color.NRGBA{0x0C, 0x1B, 0x33, 255} // midnight: background
	white = color.NRGBA{0xEA, 0xF6, 0xEF, 255} // foam: text
	mint  = color.NRGBA{0x8C, 0xF0, 0xBE, 255} // mint: accents
)

func (mono) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch n {
	case theme.ColorNameBackground, theme.ColorNameMenuBackground, theme.ColorNameOverlayBackground:
		return black
	case theme.ColorNameInputBackground, theme.ColorNameButton:
		return color.NRGBA{0x14, 0x28, 0x48, 255}
	case theme.ColorNameForeground:
		return white
	case theme.ColorNamePrimary, theme.ColorNameFocus, theme.ColorNameHyperlink, theme.ColorNameInputBorder, theme.ColorNameSeparator, theme.ColorNameScrollBar, theme.ColorNameSuccess:
		return mint
	case theme.ColorNameHover, theme.ColorNamePressed, theme.ColorNameSelection:
		return color.NRGBA{0x8C, 0xF0, 0xBE, 0x33}
	case theme.ColorNameDisabled, theme.ColorNamePlaceHolder, theme.ColorNameDisabledButton:
		return color.NRGBA{0xEA, 0xF6, 0xEF, 0x66}
	case theme.ColorNameShadow:
		return color.Transparent
	case theme.ColorNameError, theme.ColorNameWarning:
		return white
	}
	return theme.DefaultTheme().Color(n, theme.VariantDark)
}

func (mono) Font(s fyne.TextStyle) fyne.Resource     { return theme.DefaultTheme().Font(s) }
func (mono) Icon(n fyne.ThemeIconName) fyne.Resource { return theme.DefaultTheme().Icon(n) }

func (mono) Size(n fyne.ThemeSizeName) float32 {
	switch n {
	case theme.SizeNameInputRadius, theme.SizeNameSelectionRadius:
		return 0
	case theme.SizeNamePadding:
		return 6
	case theme.SizeNameText:
		return 13
	}
	return theme.DefaultTheme().Size(n)
}
