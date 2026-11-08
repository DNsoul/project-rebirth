package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/gitlab"
	"awgrotator/internal/proto"
	"awgrotator/internal/store"
	"awgrotator/internal/stores"
)

// places are the places the signed files are kept, as this tool uses them: the
// secret gist on GitHub and, when one is set up, the package on GitLab. They are
// equal. A write goes to both, and a read takes the newest of what both hold.
type places struct {
	set *stores.Set
	st  *store.State
}

// openPlaces puts the places together from the operator's state and secrets. The
// GitLab one is written with the operator's own token; without one it can only
// be read, and a write to it says so.
func openPlaces(st *store.State, secrets config.Secrets) (*places, error) {
	trust, err := st.Trust()
	if err != nil {
		return nil, err
	}
	var gl *stores.GitLabAccess
	if m, ok := st.GitLabMirror(); ok {
		gl = &stores.GitLabAccess{Base: m.Base, Project: m.ProjectID, Package: m.SyncPkg(), Token: secrets.GitLabToken}
	}
	return &places{set: stores.NewSet(trust, secrets.GitHubToken, st.GistID, gl), st: st}, nil
}

// Read returns the newest version of each named file found in any place.
func (p *places) Read(ctx context.Context, names ...string) (map[string]string, error) {
	return p.set.Read(ctx, names...)
}

// Write puts the files in every place. It succeeds when one of them takes them,
// and says out loud when another did not: the server catches that place up by
// itself, and mirror sync does it at once.
func (p *places) Write(ctx context.Context, files map[string]string) error {
	res, err := p.set.Write(ctx, files)
	if err != nil {
		return err
	}
	if failed := res.Failed(); len(failed) > 0 {
		for _, o := range failed {
			fmt.Printf("Внимание: %s не принял запись (%v).\n", o.Store, firstLine(o.Err))
		}
		fmt.Println("Записано в остальные. Отставшее место догонит сервер сам; сразу: awgctl mirror sync")
	}
	return nil
}

// firstLine shortens an error to what fits a status line.
func firstLine(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// describePlaces says what each place holds, one by one, so that a place that
// is behind or unreachable is visible at a glance.
func describePlaces(ctx context.Context, p *places) string {
	var b strings.Builder
	contentKey, err := p.st.ContentKey()
	if err != nil {
		return fmt.Sprintf("  ключ содержимого испорчен: %v\n", err)
	}
	for _, s := range p.set.Stores {
		files, err := s.Read(ctx, stores.Names...)
		if err != nil {
			fmt.Fprintf(&b, "  %s: недоступно (%s)\n", s.Name(), firstLine(err))
			continue
		}
		fmt.Fprintf(&b, "  %s:\n", s.Name())
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			plain, err := proto.Open(contentKey, files[name])
			switch {
			case err != nil:
				fmt.Fprintf(&b, "    %-2s не расшифровывается: %v\n", name, err)
			case string(plain) == "{}":
				fmt.Fprintf(&b, "    %-2s пусто\n", name)
			default:
				fmt.Fprintf(&b, "    %-2s %d байт%s\n", name, len(plain), freshness(p, name, files[name]))
			}
		}
		if len(names) == 0 {
			fmt.Fprintln(&b, "    файлов нет")
		}
	}
	return b.String()
}

// freshness names the number a file is ranked by, to compare places by eye.
func freshness(p *places, name, blob string) string {
	trust, err := p.st.Trust()
	if err != nil {
		return ""
	}
	switch name {
	case proto.FileConfig:
		if _, st, err := trust.OpenDoc(blob, proto.KindConfig, time.Now(), proto.State{}); err == nil {
			return fmt.Sprintf(", публикация №%d", st.LastSeq)
		}
	case proto.FileRelease:
		if rel, _, err := trust.OpenRelease(blob, proto.State{}); err == nil {
			return fmt.Sprintf(", сборка %d", rel.Build)
		}
	case proto.FileRevocation:
		if _, st, err := trust.OpenRevocation(blob, time.Now(), proto.State{}); err == nil {
			return fmt.Sprintf(", эпоха %d", st.MinEpoch)
		}
	}
	return ""
}

// syncPackageInfo is a line for mirror show.
func syncPackageInfo(m store.Mirror) string {
	return fmt.Sprintf("  пакет с файлами конфигурации: «%s»\n  адрес в приложениях: %s\n",
		m.SyncPkg(), gitlab.SyncTemplate(m.Base, m.ProjectID, m.SyncPkg()))
}
