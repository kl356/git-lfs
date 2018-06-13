package commands

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/git-lfs/git-lfs/errors"
	"github.com/git-lfs/git-lfs/filepathfilter"
	"github.com/git-lfs/git-lfs/git"
	"github.com/git-lfs/git-lfs/git/githistory"
	"github.com/git-lfs/git-lfs/git/odb"
	"github.com/git-lfs/git-lfs/lfs"
	"github.com/git-lfs/git-lfs/tasklog"
	"github.com/git-lfs/git-lfs/tools"
	"github.com/spf13/cobra"
)

func migrateExportCommand(cmd *cobra.Command, args []string) {
	l := tasklog.NewLogger(os.Stderr)
	defer l.Close()

	db, err := getObjectDatabase()
	if err != nil {
		ExitWithError(err)
	}
	defer db.Close()

	rewriter := getHistoryRewriter(cmd, db, l)

	filter := rewriter.Filter()
	if len(filter.Include()) <= 0 {
		ExitWithError(errors.Errorf("fatal: one or more files must be specified with --include"))
	}

	tracked := trackedFromExportFilter(filter)
	gitfilter := lfs.NewGitFilter(cfg)

	migrate(args, rewriter, l, &githistory.RewriteOptions{
		Verbose:           migrateVerbose,
		ObjectMapFilePath: objectMapFilePath,
		BlobFn: func(path string, b *odb.Blob) (*odb.Blob, error) {
			if filepath.Base(path) == ".gitattributes" {
				return b, nil
			}

			var buf bytes.Buffer

			if _, err := smudge(gitfilter, &buf, b.Contents, path, false, rewriter.Filter()); err != nil {
				return nil, err
			}

			return &odb.Blob{
				Contents: &buf, Size: int64(buf.Len()),
			}, nil
		},

		TreeCallbackFn: func(path string, t *odb.Tree) (*odb.Tree, error) {
			if path != "/" {
				// Ignore non-root trees.
				return t, nil
			}

			ours := tracked
			theirs, err := trackedFromAttrs(db, t)
			if err != nil {
				return nil, err
			}

			// Create a blob of the attributes that are optionally
			// present in the "t" tree's .gitattributes blob, and
			// union in the patterns that we've tracked.
			//
			// Perform this Union() operation each time we visit a
			// root tree such that if the underlying .gitattributes
			// is present and has a diff between commits in the
			// range of commits to migrate, those changes are
			// preserved.
			blob, err := trackedToBlob(db, theirs.Clone().Union(ours))
			if err != nil {
				return nil, err
			}

			// Finally, return a copy of the tree "t" that has the
			// new .gitattributes file included/replaced.
			return t.Merge(&odb.TreeEntry{
				Name:     ".gitattributes",
				Filemode: 0100644,
				Oid:      blob,
			}), nil
		},

		UpdateRefs: true,
	})

	// Only perform `git-checkout(1) -f` if the repository is
	// non-bare.
	if bare, _ := git.IsBare(); !bare {
		t := l.Waiter("migrate: checkout")
		err := git.Checkout("", nil, true)
		t.Complete()

		if err != nil {
			ExitWithError(err)
		}
	}
}

// trackedFromExportFilter returns an ordered set of strings where each entry is a
// line in the .gitattributes file. It adds/removes the fiter/diff/merge=lfs
// attributes based on patterns included/excluded in the given filter. Since
// `migrate export` removes files from Git LFS, it will remove attributes for included
// files, and add attributes for excluded files
func trackedFromExportFilter(filter *filepathfilter.Filter) *tools.OrderedSet {
	tracked := tools.NewOrderedSet()

	for _, include := range filter.Include() {
		tracked.Add(fmt.Sprintf("%s text -filter -merge -diff", escapeAttrPattern(include)))
	}

	for _, exclude := range filter.Exclude() {
		tracked.Add(fmt.Sprintf("%s filter=lfs diff=lfs merge=lfs -text", escapeAttrPattern(exclude)))
	}

	return tracked
}