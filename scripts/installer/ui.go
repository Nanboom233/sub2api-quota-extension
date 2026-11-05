package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/pterm/pterm"
	"golang.org/x/term"
)

func (i *installer) prepareInput() {
	if i.lines != nil {
		return
	}
	i.lines = make(chan lineInput, 1)
	go func() {
		defer close(i.lines)
		reader := bufio.NewReader(i.in)
		for {
			line, err := reader.ReadString('\n')
			i.lines <- lineInput{line, err}
			if err != nil {
				return
			}
		}
	}()
}

type terminalUI struct {
	chinese bool
	dynamic bool
	spinner *pterm.SpinnerPrinter
	area    *pterm.AreaPrinter
}

type progressArea struct{ area *pterm.AreaPrinter }

func (w progressArea) Write(frame []byte) (int, error) {
	text := strings.TrimSpace(string(frame))
	if text != "" {
		w.area.Update(text)
	}
	return len(frame), nil
}

func chineseLocale(getenv func(string) string) bool {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		value := strings.TrimSpace(getenv(key))
		if value == "" {
			continue
		}
		value = strings.ToLower(strings.ReplaceAll(value, "-", "_"))
		value = strings.SplitN(strings.SplitN(value, ".", 2)[0], "@", 2)[0]
		return value == "zh" || strings.HasPrefix(value, "zh_")
	}
	return false
}

func newTerminalUI(in io.Reader, out io.Writer) *terminalUI {
	return &terminalUI{
		chinese: chineseLocale(os.Getenv),
		dynamic: in == os.Stdin && out == os.Stdout && os.Getenv("TERM") != "dumb" &&
			term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())),
	}
}

func (i *installer) msg(message string) string {
	if i.ui != nil && i.ui.chinese {
		if translated, ok := chineseMessages[message]; ok {
			return translated
		}
	}
	return message
}

func (i *installer) choose(ctx context.Context, options []string) (int, error) {
	i.stopProgress()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if i.ui != nil && i.ui.dynamic {
		promptCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		selected, err := pterm.DefaultInteractiveSelect.WithOptions(options).WithFilter(false).
			WithOnInterruptFunc(cancel).Show(i.msg("Select"))
		if promptCtx.Err() != nil {
			return 0, promptCtx.Err()
		}
		if err != nil {
			return 0, errors.New(i.msg("Terminal input failed."))
		}
		for n, option := range options {
			if selected == option {
				return n + 1, nil
			}
		}
		return 0, errors.New(i.msg("Installation canceled."))
	}
	for _, option := range options {
		fmt.Fprintln(i.out, option)
	}
	for {
		fmt.Fprint(i.out, i.msg("Select: "))
		line, err := i.readLine(ctx)
		if err != nil {
			return 0, err
		}
		n, err := strconv.Atoi(line)
		if err == nil && n >= 1 && n <= len(options) {
			return n, nil
		}
	}
}

func (i *installer) input(ctx context.Context, label, defaultValue string) (string, error) {
	i.stopProgress()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if i.ui != nil && i.ui.dynamic {
		promptCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		value, err := pterm.DefaultInteractiveTextInput.WithDefaultValue(defaultValue).
			WithOnInterruptFunc(cancel).Show(i.msg(label))
		if promptCtx.Err() != nil {
			return "", promptCtx.Err()
		}
		if err != nil {
			return "", errors.New(i.msg("Terminal input failed."))
		}
		return strings.TrimSpace(value), nil
	}
	fmt.Fprintf(i.out, "%s [%s]: ", i.msg(label), defaultValue)
	value, err := i.readLine(ctx)
	if err == nil && value == "" {
		value = defaultValue
	}
	return value, err
}

func (i *installer) askConfirmation(ctx context.Context, question string) error {
	i.stopProgress()
	if err := ctx.Err(); err != nil {
		return err
	}
	if i.ui != nil && i.ui.dynamic {
		promptCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		accepted, err := pterm.DefaultInteractiveConfirm.WithDefaultValue(false).
			WithConfirmText(i.msg("Y continue")).WithRejectText(i.msg("N cancel")).
			WithOnInterruptFunc(cancel).Show(i.msg(question))
		if promptCtx.Err() != nil {
			return promptCtx.Err()
		}
		if err != nil {
			return errors.New(i.msg("Terminal input failed."))
		}
		if accepted {
			return nil
		}
		return errors.New(i.msg("Installation canceled."))
	}
	fmt.Fprintf(i.out, "%s [y/N] ", i.msg(question))
	line, err := i.readLine(ctx)
	if err != nil {
		return fmt.Errorf(i.msg("Installation canceled: %w"), err)
	}
	if strings.EqualFold(line, "y") || strings.EqualFold(line, "yes") {
		return nil
	}
	return errors.New(i.msg("Installation canceled."))
}

func (i *installer) startProgress(message string) {
	i.stopProgress()
	if i.ui != nil && i.ui.dynamic {
		i.ui.area, _ = pterm.DefaultArea.Start()
		i.ui.spinner, _ = pterm.DefaultSpinner.WithShowTimer(true).WithWriter(progressArea{i.ui.area}).Start(i.msg(message))
		return
	}
	fmt.Fprintln(i.out, i.msg(message))
}

func (i *installer) progressDetail(message string, values ...any) {
	if i.ui != nil && i.ui.spinner != nil {
		return
	}
	fmt.Fprintf(i.out, message+"\n", values...)
}

func (i *installer) stopProgress() {
	if i.ui != nil && i.ui.spinner != nil {
		_ = i.ui.spinner.Stop()
		i.ui.spinner = nil
		_ = i.ui.area.Stop()
		i.ui.area = nil
		fmt.Fprintln(i.out)
	}
}
