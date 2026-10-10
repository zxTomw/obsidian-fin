package export

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"obsidian-fin/internal/domain"
	"obsidian-fin/internal/errs"
)

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, ErrConflict
	}
	return b, nil
}

type Repository interface {
	Notes(context.Context) ([]domain.Note, error)
	BeginExport(context.Context, string, string, string, int) error
	FinishExport(context.Context, string, string) error
}
type Counts struct{ Created, Updated, Unchanged, Conflicted, Failed int }

func RelativePath(n domain.Note) (string, error) {
	if !opaqueID.MatchString(n.ID) || (n.Environment != "sandbox" && n.Environment != "production") {
		return "", errors.New("invalid note identity or environment")
	}
	return filepath.ToSlash(filepath.Join("Finance", "obsidian-fin", n.Environment, ItemHash(n.ItemID), n.ID+".md")), nil
}

func Run(ctx context.Context, repo Repository, files Files, progress io.Writer) (Counts, error) {
	notes, err := repo.Notes(ctx)
	if err != nil {
		return Counts{}, errs.Wrap(1, "could not read a consistent export snapshot", err)
	}
	var counts Counts
	for _, n := range notes {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		path, err := RelativePath(n)
		if err != nil {
			return counts, errs.Wrap(1, "invalid stored note identity", err)
		}
		outcome, err := exportNote(ctx, repo, files, n, path)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return counts, err
			}
			if errors.Is(err, ErrConflict) {
				counts.Conflicted++
				fmt.Fprintf(progress, "Conflict: %s\n", path)
			} else {
				counts.Failed++
				fmt.Fprintf(progress, "Could not export: %s; rerun export\n", path)
			}
			continue
		}
		switch outcome {
		case "created":
			counts.Created++
		case "updated":
			counts.Updated++
		default:
			counts.Unchanged++
		}
	}
	if counts.Conflicted > 0 || counts.Failed > 0 {
		code := 3
		if counts.Failed > 0 && counts.Created+counts.Updated+counts.Unchanged+counts.Conflicted == 0 {
			code = 1
		}
		return counts, errs.New(code, "export incomplete; conflicted files were preserved; move generated-block edits into Notes and restore the prior block before retrying; see README for recovery")
	}
	return counts, nil
}
func matches(hash string, allowed *string) bool { return allowed != nil && hash == *allowed }
func exportNote(ctx context.Context, repo Repository, files Files, n domain.Note, path string) (string, error) {
	if n.Export.Path != "" && n.Export.Path != path {
		return "", ErrConflict
	}
	before, err := files.Read(path)
	if err != nil {
		return "", err
	}
	block := Render(n)
	desired := Hash(block)
	content := Initial(block)
	outcome := "created"
	if before.Exists {
		if n.Export.Path == "" || (n.Export.BlockHash == nil && n.Export.PendingHash == nil) {
			return "", ErrConflict
		}
		i, j, valid := BlockBounds(before.Bytes, n.ID)
		if !valid {
			return "", ErrConflict
		}
		current := Hash(before.Bytes[i:j])
		if current != desired && !matches(current, n.Export.BlockHash) && !matches(current, n.Export.PendingHash) {
			return "", ErrConflict
		}
		if matches(current, n.Export.PendingHash) && !matches(current, n.Export.BlockHash) {
			// Promote the recognized interrupted write before replacing its pending hash.
			// Otherwise another failure before writing would forget the only accepted block.
			if err := repo.FinishExport(ctx, n.ID, current); err != nil {
				return "", err
			}
		}
		if current == desired {
			if !matches(current, n.Export.BlockHash) || n.Export.PendingHash != nil {
				if err := repo.BeginExport(ctx, n.ID, path, desired, TemplateVersion); err != nil {
					return "", err
				}
				if err := repo.FinishExport(ctx, n.ID, desired); err != nil {
					return "", err
				}
			}
			return "unchanged", nil
		}
		content = make([]byte, 0, len(before.Bytes)+len(block))
		content = append(content, before.Bytes[:i]...)
		content = append(content, block...)
		content = append(content, before.Bytes[j:]...)
		outcome = "updated"
	} else if n.Export.ExportedAt != nil || n.Export.BlockHash != nil {
		return "", ErrConflict
	}
	if err := repo.BeginExport(ctx, n.ID, path, desired, TemplateVersion); err != nil {
		return "", err
	}
	if err := files.WriteAtomic(path, before, content); err != nil {
		return "", err
	}
	if err := repo.FinishExport(ctx, n.ID, desired); err != nil {
		return "", err
	}
	return outcome, nil
}
