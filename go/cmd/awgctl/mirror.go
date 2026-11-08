package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
	"time"

	"awgrotator/internal/config"
	"awgrotator/internal/gitlab"
	"awgrotator/internal/proto"
	"awgrotator/internal/store"
)

// ---- mirror

func cmdMirror(args []string) error {
	if len(args) == 0 {
		return errors.New("нужно указать: set | show")
	}
	switch args[0] {
	case "set":
		return cmdMirrorSet(args[1:])
	case "show":
		return cmdMirrorShow(args[1:])
	}
	return fmt.Errorf("неизвестная подкоманда %q: set | show", args[0])
}

// setMirror looks the project up and returns the mirror that names it. It makes
// sure the project is there and public before anything is built around it.
func setMirror(ctx context.Context, base, projectPath, pkg, token string) (store.Mirror, error) {
	if base == "" {
		base = gitlab.DefaultBase
	}
	if pkg == "" {
		pkg = gitlab.DefaultPackage
	}
	id, err := gitlab.New(base, token).ResolveProject(ctx, projectPath)
	if err != nil {
		return store.Mirror{}, err
	}
	return store.Mirror{
		Kind:      store.KindGitLab,
		Base:      strings.TrimRight(base, "/"),
		ProjectID: id,
		Path:      projectPath,
		Package:   pkg,
	}, nil
}

func cmdMirrorSet(args []string) error {
	if len(args) == 0 || args[0] != store.KindGitLab {
		return errors.New("пока есть один вид зеркала: gitlab. Например: awgctl mirror set gitlab -project владелец/имя")
	}
	fs := flag.NewFlagSet("mirror set gitlab", flag.ExitOnError)
	project := fs.String("project", "", "публичный проект на GitLab, владелец/имя")
	base := fs.String("url", gitlab.DefaultBase, "адрес GitLab")
	pkg := fs.String("package", gitlab.DefaultPackage, "имя пакета, под которым лежат установщики")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *project == "" {
		return errors.New("нужен -project владелец/имя")
	}
	root, err := config.Root()
	if err != nil {
		return err
	}
	st, err := store.Load(root)
	if err != nil {
		return err
	}
	secrets, err := config.LoadSecrets(root)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m, err := setMirror(ctx, *base, *project, *pkg, secrets.GitLabToken)
	if err != nil {
		return err
	}
	st.SetMirror(m)
	if err := st.Save(root); err != nil {
		return err
	}
	fmt.Printf("Запасное место: GitLab, проект %s (id %d), пакет «%s».\n", m.Path, m.ProjectID, m.Package)
	fmt.Println("Установщики при выпуске будут загружаться и туда (release publish), если есть GITLAB_TOKEN в secrets.env.")
	fmt.Println("Чтобы приложения знали об этом месте, их нужно пересобрать: адрес попадает в app/assets/bootstrap.json (awgctl trust -o).")
	return nil
}

func cmdMirrorShow(args []string) error {
	fs := flag.NewFlagSet("mirror show", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := config.Root()
	if err != nil {
		return err
	}
	st, err := store.Load(root)
	if err != nil {
		return err
	}
	secrets, _ := config.LoadSecrets(root)
	fmt.Print(describeMirrors(st))
	if len(st.Mirrors) > 0 {
		if secrets.GitLabToken == "" {
			fmt.Println("GITLAB_TOKEN в secrets.env не задан: загрузить файлы на GitLab нельзя.")
		} else {
			fmt.Println("GITLAB_TOKEN задан.")
		}
	}
	return nil
}

func describeMirrors(st *store.State) string {
	if len(st.Mirrors) == 0 {
		return "Запасных мест нет. Добавить: awgctl mirror set gitlab -project владелец/имя\n"
	}
	var b strings.Builder
	for _, m := range st.Mirrors {
		fmt.Fprintf(&b, "Запасное место: %s, проект %s (id %d), пакет «%s»\n", m.Kind, m.Path, m.ProjectID, m.Package)
	}
	for _, t := range st.ReleaseMirrors() {
		fmt.Fprintf(&b, "  адрес в приложениях: %s\n", t)
	}
	return b.String()
}

// announcedName is the name of the file in an announced address, which is the
// name an app will ask the mirrors for.
func announcedName(announced string) string {
	u, err := url.Parse(announced)
	if err != nil {
		return ""
	}
	return path.Base(u.Path)
}

// uploadToMirrors puts every installer of the release on every mirror, under the
// name the announced address gives it, and then fetches each copy the way an app
// would, without a token, to see that it is really there and is the file the
// announcement will name. Nothing is announced until that has worked.
func uploadToMirrors(ctx context.Context, out io.Writer, st *store.State, token string, rel *proto.Release, files []releaseFile) error {
	for _, m := range st.Mirrors {
		if m.Kind != store.KindGitLab {
			continue
		}
		c := gitlab.New(m.Base, token)
		for _, f := range files {
			asset, ok := rel.Asset(f.OS)
			if !ok {
				continue
			}
			name := announcedName(asset.URL)
			if name == "" || name == "." || name == "/" {
				return fmt.Errorf("%s: в адресе %q нет имени файла", f.OS, asset.URL)
			}
			fmt.Fprintf(out, "  %-8s → GitLab %s: загружаю %s…\n", f.OS, m.Path, name)
			addr, err := c.UploadFile(ctx, m.ProjectID, m.Package, rel.Version, name, f.Path)
			if err != nil {
				return fmt.Errorf("зеркало %s: %w", m.Path, err)
			}
			if err := gitlab.CheckDownload(ctx, nil, addr, asset.SHA256, asset.Size); err != nil {
				return fmt.Errorf("зеркало %s: %w", m.Path, err)
			}
			fmt.Fprintf(out, "           на месте и совпадает: %s\n", addr)
		}
	}
	return nil
}

// bootstrapBundle is what an app build is made with.
type bootstrapBundle struct {
	proto.Trust
	GistID    string `json:"gist_id"`
	GistOwner string `json:"gist_owner"`

	// ReleaseMirrors are templates of the addresses of other copies of the
	// installers, with {version} and {file} for the app to fill in.
	ReleaseMirrors []string `json:"release_mirrors,omitempty"`
}

func newBootstrapBundle(trust proto.Trust, st *store.State) bootstrapBundle {
	return bootstrapBundle{
		Trust:          trust,
		GistID:         st.GistID,
		GistOwner:      st.GistOwner,
		ReleaseMirrors: st.ReleaseMirrors(),
	}
}
