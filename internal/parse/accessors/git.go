package accessors

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// GitAccessor clones a git repository into a temp directory. It handles
// "git+https://", "git://", "https://github.com/...", and "git@github.com:..."
// source forms.
type GitAccessor struct{}

func init() {
	parse.RegisterAccessor("git", func() (parse.DataAccessor, error) {
		return &GitAccessor{}, nil
	})
	parse.RegisterAccessor("git+https", func() (parse.DataAccessor, error) {
		return &GitAccessor{}, nil
	})
	parse.RegisterAccessor("git+ssh", func() (parse.DataAccessor, error) {
		return &GitAccessor{}, nil
	})
}

// CanHandle reports whether source looks like a git URL.
func (a *GitAccessor) CanHandle(source string, opts parse.AccessorOptions) bool {
	s := parse.SchemeOf(source)
	if s == "git" || s == "git+https" || s == "git+ssh" {
		return true
	}
	if strings.HasPrefix(source, "git@") {
		return true
	}
	// Heuristic: github.com / gitlab.com / bitbucket.org URLs that end in
	// .git or contain "/blob/" or "/tree/" are git sources.
	if strings.HasPrefix(source, "https://github.com/") ||
		strings.HasPrefix(source, "https://gitlab.com/") ||
		strings.HasPrefix(source, "https://bitbucket.org/") {
		return strings.HasSuffix(source, ".git") ||
			strings.Contains(source, "/blob/") ||
			strings.Contains(source, "/tree/")
	}
	return false
}

// Schemes returns git, git+https, git+ssh.
func (a *GitAccessor) Schemes() []string { return []string{"git", "git+https", "git+ssh"} }

// Fetch clones the repository (or a sub-path / branch) into a temp
// directory. Supports the fragment-suffix convention to address a
// sub-path: "https://github.com/foo/bar.git/sub/path" clones the repo
// and returns the sub-path inside it.
func (a *GitAccessor) Fetch(ctx context.Context, source string, opts parse.AccessorOptions) (*parse.LocalResource, error) {
	url, branch, subPath := parseGitURL(source)
	tmp, err := os.MkdirTemp(opts.TemporaryDir, "openviking-git-*")
	if err != nil {
		return nil, domain.Wrap(domain.CodeInternalError, 500, err)
	}
	cloneOpts := &git.CloneOptions{
		URL:      url,
		Depth:    1,
		Progress: nil,
	}
	if branch != "" {
		cloneOpts.ReferenceName = plumbing.NewBranchReferenceName(branch)
		cloneOpts.SingleBranch = true
	}
	if _, err := git.PlainCloneContext(ctx, tmp, false, cloneOpts); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, domain.Wrap(domain.CodeInternalError, 502,
			fmt.Errorf("git clone %s: %w", url, err))
	}
	resolved := tmp
	if subPath != "" {
		resolved = filepath.Join(tmp, subPath)
		if _, err := os.Stat(resolved); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, domain.Wrap(domain.CodeResourceNotFound, 404,
				fmt.Errorf("git sub-path %s: %w", subPath, err))
		}
	}
	return &parse.LocalResource{
		Path:           resolved,
		SourceType:     parse.SourceGit,
		OriginalSource: source,
		IsTemporary:    true,
		Meta: map[string]any{
			"repo_url":  url,
			"branch":    branch,
			"sub_path":  subPath,
			"clone_dir": tmp,
		},
	}, nil
}

// parseGitURL splits a source string into (url, branch, subPath).
//
// Forms:
//   - "git+https://host/repo.git@branch/sub/path"
//   - "https://host/repo.git/sub/path"
//   - "git@host:org/repo.git" (no scheme; recognized by GitAccessor)
func parseGitURL(source string) (url, branch, subPath string) {
	url = source
	// Strip "git+" prefix from scheme.
	if strings.HasPrefix(url, "git+") {
		url = strings.TrimPrefix(url, "git+")
	}
	// Branch is after "@" in the URL path component (not the userinfo "@").
	// We only honor "@branch" when it appears after ".git" or after a path
	// segment that does not look like a user@host pair.
	if idx := strings.Index(url, ".git@"); idx >= 0 {
		rest := url[idx+len(".git@"):]
		// branch is everything up to the next "/".
		slash := strings.IndexByte(rest, '/')
		if slash >= 0 {
			branch = rest[:slash]
			subPath = rest[slash+1:]
			url = url[:idx+len(".git")]
		} else {
			branch = rest
			url = url[:idx+len(".git")]
		}
	} else if strings.HasSuffix(url, ".git") {
		// No branch; nothing to do.
	} else if i := strings.Index(url, "/blob/"); i >= 0 {
		// Convert GitHub web URL to a repo + sub-path.
		rest := url[i+len("/blob/"):]
		slash := strings.IndexByte(rest, '/')
		if slash >= 0 {
			branch = rest[:slash]
			subPath = rest[slash+1:]
		} else {
			branch = rest
		}
		url = url[:i] + ".git"
	} else if i := strings.Index(url, "/tree/"); i >= 0 {
		rest := url[i+len("/tree/"):]
		slash := strings.IndexByte(rest, '/')
		if slash >= 0 {
			branch = rest[:slash]
			subPath = rest[slash+1:]
		} else {
			branch = rest
		}
		url = url[:i] + ".git"
	}
	// Sub-path may also appear as a trailing path after a slash when no
	// branch is specified. We already captured that above.
	return url, branch, subPath
}
