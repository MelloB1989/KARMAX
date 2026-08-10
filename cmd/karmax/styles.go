package main

import "github.com/charmbracelet/lipgloss"

// Terminal styles, shared by the commands that print for a person to read.
// They lived in the loops TUI until that was retired with the go-get installer.
var (
	stAmber = lipgloss.NewStyle().Foreground(lipgloss.Color("#d9a441"))
	stMuted = lipgloss.NewStyle().Foreground(lipgloss.Color("#8a93a0"))
	stRed   = lipgloss.NewStyle().Foreground(lipgloss.Color("#e06c75"))
	stGreen = lipgloss.NewStyle().Foreground(lipgloss.Color("#98c379"))
	stBold  = lipgloss.NewStyle().Bold(true)
)
