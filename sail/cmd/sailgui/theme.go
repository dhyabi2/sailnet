package main

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// mono is the Sailnet look: black, white, nothing else. No greys except the
// disabled state, no rounded corners, no shadows.
type mono struct{}

var (
	black = color.Black
	white = color.White
)

func (mono) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch n {
	case theme.ColorNameBackground, theme.ColorNameInputBackground, theme.ColorNameMenuBackground, theme.ColorNameOverlayBackground:
		return white
	case theme.ColorNameForeground, theme.ColorNamePrimary, theme.ColorNameFocus, theme.ColorNameHyperlink, theme.ColorNameInputBorder, theme.ColorNameSeparator, theme.ColorNameScrollBar:
		return black
	case theme.ColorNameButton:
		return white
	case theme.ColorNameHover, theme.ColorNamePressed, theme.ColorNameSelection:
		return color.NRGBA{0, 0, 0, 0x22}
	case theme.ColorNameDisabled, theme.ColorNamePlaceHolder, theme.ColorNameDisabledButton:
		return color.NRGBA{0, 0, 0, 0x66}
	case theme.ColorNameShadow:
		return color.Transparent
	case theme.ColorNameSuccess, theme.ColorNameError, theme.ColorNameWarning:
		return black
	}
	return theme.DefaultTheme().Color(n, theme.VariantLight)
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
