package main

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// mergeCSV folds the rows of src (a newer bundle's file) into dst, the file the client imports; dst is created
// when missing. A src row replaces the dst row with the same key in place; new keys are appended. Merging the
// same src twice gives the same file. Both are written the way the server wrote src (BOM, line endings).
func mergeCSV(dst, src string) error {
	newer, err := readCSV(src)
	if err != nil {
		return fmt.Errorf("%s from the server: %w", filepath.Base(src), err)
	}
	older, err := readCSV(dst)
	if errors.Is(err, os.ErrNotExist) {
		return writeCSV(dst, newer)
	}
	if err != nil {
		return fmt.Errorf("%s: %w (delete it if it is already imported, then run this again)", dst, err)
	}
	if !slices.Equal(older.header, newer.header) {
		if older.rows, err = reshape(older, newer.header); err != nil {
			// A column was removed or renamed: the two can't be one file. Nothing is merged or marked downloaded.
			return fmt.Errorf("the columns of %s changed. Import it into Bitrix24 and delete it, then run this again", filepath.Base(dst))
		}
	}

	key := keyFunc(newer.header)
	numbered := newer.header[0] == "ID"
	merged := records(older.rows, numbered)
	at := map[string]int{}
	for i, rec := range merged {
		at[key(rec[0])] = i
	}
	for _, rec := range records(newer.rows, numbered) {
		k := key(rec[0])
		if i, ok := at[k]; ok {
			merged[i] = rec
		} else {
			at[k] = len(merged)
			merged = append(merged, rec)
		}
	}
	newer.rows = nil
	for i, rec := range merged {
		for _, row := range rec {
			if numbered {
				row[0] = strconv.Itoa(i + 1) // every bundle numbers its deals from 1: one ID per deal in the merged file
			}
			newer.rows = append(newer.rows, row)
		}
	}
	return writeCSV(dst, newer)
}

// records groups rows into what Bitrix imports as one record. In a numbered file (deals, first column "ID") a
// deal is all the consecutive rows with its ID: its own fields on the first, one product per row. Otherwise a row.
func records(rows [][]string, numbered bool) [][][]string {
	var out [][][]string
	for i, row := range rows {
		if numbered && i > 0 && row[0] == rows[i-1][0] {
			out[len(out)-1] = append(out[len(out)-1], row)
		} else {
			out = append(out, [][]string{row})
		}
	}
	return out
}

// reshape moves t's rows into a header that only adds columns to t's (new columns empty). The server added one:
// rows downloaded before it still merge with the newer ones. A removed or renamed column is an error.
func reshape(t table, header []string) ([][]string, error) {
	from := make([]int, len(header))
	for i, name := range header {
		from[i] = slices.Index(t.header, name)
	}
	for _, name := range t.header {
		if !slices.Contains(header, name) {
			return nil, fmt.Errorf("column %q is gone", name)
		}
	}
	rows := make([][]string, len(t.rows))
	for r, old := range t.rows {
		rows[r] = make([]string, len(header))
		for i, j := range from {
			if j >= 0 {
				rows[r][i] = old[j]
			}
		}
	}
	return rows, nil
}

// keyFunc picks what identifies a record by its first row: products have an External ID and deals a Deal key;
// contacts a Contact key; companies carry theirs at the start of Comment ("Key: IVF-CO-501; ..."), as did contacts
// before 2026-10-19 (so a contact without a Contact key yet is found by its Comment). Otherwise the whole row.
func keyFunc(header []string) func([]string) string {
	for _, name := range []string{"External ID", "Deal key"} {
		if i := slices.Index(header, name); i >= 0 {
			return func(row []string) string { return row[i] }
		}
	}
	contact, comment := slices.Index(header, "Contact key"), slices.Index(header, "Comment")
	return func(row []string) string {
		if contact >= 0 && row[contact] != "" {
			return "key\x00" + row[contact]
		}
		if comment >= 0 {
			if k, ok := strings.CutPrefix(row[comment], "Key: "); ok {
				k, _, _ = strings.Cut(k, ";")
				return "key\x00" + k
			}
		}
		return "row\x00" + strings.Join(row, "\x00")
	}
}

type table struct {
	header []string
	rows   [][]string
	bom    bool
	crlf   bool
}

func readCSV(path string) (table, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return table{}, err
	}
	t := table{crlf: bytes.Contains(data, []byte("\r\n"))}
	data, t.bom = bytes.CutPrefix(data, []byte("\ufeff"))
	r := csv.NewReader(bytes.NewReader(data))
	if t.header, err = r.Read(); err != nil {
		if err == io.EOF {
			return t, errors.New("the file is empty")
		}
		return t, err
	}
	for {
		row, err := r.Read()
		if err == io.EOF {
			return t, nil
		}
		if err != nil {
			return t, err
		}
		t.rows = append(t.rows, row)
	}
}

// writeCSV writes next to path, then renames, so the client never sees a half-written file.
func writeCSV(path string, t table) error {
	var buf bytes.Buffer
	if t.bom {
		buf.WriteString("\ufeff")
	}
	w := csv.NewWriter(&buf)
	w.UseCRLF = t.crlf
	w.Write(t.header)
	w.WriteAll(t.rows)
	if err := w.Error(); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return lockedError(path, err)
	}
	return nil
}

// lockedError: on Windows a file open in Excel can't be replaced.
func lockedError(path string, err error) error {
	return fmt.Errorf("could not update %s. If it is open (in Excel, for example), close it and run this again (%w)", filepath.Base(path), err)
}

// countRows: records in a CSV file, not counting the header: rows, or deals in a deals file (several rows each).
func countRows(path string) (int, error) {
	t, err := readCSV(path)
	if err != nil {
		return 0, fmt.Errorf("%s: %w (delete it if it is already imported, then run this again)", filepath.Base(path), err)
	}
	return len(records(t.rows, t.header[0] == "ID")), nil
}
