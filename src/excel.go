package main

import (
	"archive/zip"
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Excel copies of the production files, only for looking at the data: a double-click on a CSV file shows garbled
// Georgian, one column or broken barcodes, depending on the computer's settings. The CSV files stay what goes into
// Bitrix. Only the standard library: a .xlsx file is a zip of a few XML files.

// numberColumns are shown as numbers, so they add up. Everything else stays text: Excel would otherwise break
// barcodes (4.8E+12), and drop the leading zeros of phones and codes.
var numberColumns = map[string]bool{"Price": true, "Amount": true, "Quantity": true, "Items": true, "Purchases": true,
	"Total spend": true, "Average purchase": true, "Purchases (last year)": true, "Spend (last year)": true,
	"Returns": true, "Card discount %": true}

// maxCellChars: Excel refuses a file with a longer cell (its limit is 32,767).
const maxCellChars = 32000

// maxSheetRows is Excel's limit, the header included.
const maxSheetRows = 1048576

var plainNumber = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// cleanExcel removes the copy of a production file the client has deleted: it holds customer data. Only the program's
// own files are touched, never anything else someone saved in the folder. It returns the copies it could not remove
// (open in Excel).
func cleanExcel(production, excel string) (stuck []string) {
	for _, name := range importOrder {
		if _, err := os.Stat(filepath.Join(production, name)); !os.IsNotExist(err) {
			continue
		}
		copyName := strings.TrimSuffix(name, ".csv") + ".xlsx"
		path := filepath.Join(excel, copyName)
		if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(path); err != nil {
			stuck = append(stuck, copyName)
		}
	}
	// A temporary file left by a crash; a young one may belong to another Lutecia window.
	leftovers, _ := filepath.Glob(filepath.Join(excel, ".*.xlsx.*.tmp"))
	for _, path := range leftovers {
		if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() && time.Since(fi.ModTime()) > time.Hour {
			os.Remove(path)
		}
	}
	return stuck
}

// syncExcel makes a copy of each production file that is new or changed. One copy failing (open in Excel) does not
// stop the others. ready is true when every production file has a current copy.
func syncExcel(production, excel string, files []fileRows) (ready bool, err error) {
	if len(files) == 0 {
		return false, nil
	}
	if err := os.MkdirAll(excel, 0o755); err != nil {
		return false, err
	}
	var failed []string
	for _, f := range files {
		src := filepath.Join(production, f.Name)
		copyName := strings.TrimSuffix(f.Name, ".csv") + ".xlsx"
		if err := syncOne(src, filepath.Join(excel, copyName)); err != nil {
			failed = append(failed, copyName)
		}
	}
	if len(failed) > 0 {
		return false, fmt.Errorf("%s may still show older data: close them in Excel and run this again", strings.Join(failed, ", "))
	}
	return true, nil
}

// syncOne: a copy carries the time of its CSV file, so any other time means it is out of date or was changed.
func syncOne(src, dst string) error {
	s, err := os.Stat(src)
	if err != nil {
		return err
	}
	if d, err := os.Stat(dst); err == nil && d.Mode().IsRegular() && d.ModTime().Equal(s.ModTime()) {
		return nil
	}
	t, err := readCSV(src)
	if err != nil {
		return err
	}
	if err := writeXLSX(dst, t); err != nil {
		return err
	}
	return os.Chtimes(dst, s.ModTime(), s.ModTime())
}

// writeXLSX writes next to path, then renames, so a half-written file is never seen. The temporary name is unique:
// two Lutecia windows at once must not write into one file.
func writeXLSX(path string, t table) error {
	if len(t.rows)+1 > maxSheetRows {
		return fmt.Errorf("%s: %d rows do not fit in Excel", filepath.Base(path), len(t.rows))
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	err = encodeXLSX(f, t)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return lockedError(path, err)
	}
	return nil
}

func encodeXLSX(w io.Writer, t table) error {
	z := zip.NewWriter(w)
	for _, part := range []struct{ name, body string }{
		{"[Content_Types].xml", xmlHead + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
			`<Default Extension="xml" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>` +
			`<Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/></Types>`},
		{"_rels/.rels", xmlHead + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`},
		{"xl/workbook.xml", xmlHead + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" ` +
			`xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><bookViews><workbookView/></bookViews><sheets><sheet name="Data" sheetId="1" r:id="rId1"/></sheets></workbook>`},
		{"xl/_rels/workbook.xml.rels", xmlHead + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
			`<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>` +
			`<Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/></Relationships>`},
		{"xl/styles.xml", xmlHead + `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
			`<fonts count="2"><font><sz val="11"/><name val="Calibri"/></font><font><b/><sz val="11"/><name val="Calibri"/></font></fonts>` +
			`<fills count="2"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill></fills>` +
			`<borders count="1"><border><left/><right/><top/><bottom/><diagonal/></border></borders>` +
			`<cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs>` +
			`<cellXfs count="2"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0" applyFont="1"/></cellXfs>` +
			`</styleSheet>`},
	} {
		p, err := z.Create(part.name)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(p, part.body); err != nil {
			return err
		}
	}
	p, err := z.Create("xl/worksheets/sheet1.xml")
	if err != nil {
		return err
	}
	b := bufio.NewWriter(p)
	b.WriteString(xmlHead + `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<sheetViews><sheetView workbookViewId="0"><pane ySplit="1" topLeftCell="A2" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews>` +
		`<sheetFormatPr defaultRowHeight="15"/><cols><col min="1" max="` + strconv.Itoa(max(len(t.header), 1)) + `" width="18" customWidth="1"/></cols><sheetData>`)
	numeric := make([]bool, len(t.header))
	letters := make([]string, len(t.header))
	for i, name := range t.header {
		numeric[i], letters[i] = numberColumns[name], columnLetters(i)
	}
	writeRow(b, 1, t.header, letters, nil, true)
	for i, row := range t.rows {
		writeRow(b, i+2, row, letters, numeric, false)
	}
	b.WriteString(`</sheetData></worksheet>`)
	if err := b.Flush(); err != nil {
		return err
	}
	return z.Close()
}

const xmlHead = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n"

func writeRow(b *bufio.Writer, n int, row, letters []string, numeric []bool, bold bool) {
	fmt.Fprintf(b, `<row r="%d">`, n)
	style := ""
	if bold {
		style = ` s="1"`
	}
	for i, cell := range row {
		if cell == "" || i >= len(letters) {
			continue
		}
		if numeric != nil && numeric[i] {
			if plainNumber.MatchString(cell) {
				fmt.Fprintf(b, `<c r="%s%d"><v>%s</v></c>`, letters[i], n, cell)
				continue
			}
		}
		fmt.Fprintf(b, `<c r="%s%d" t="inlineStr"%s><is><t xml:space="preserve">`, letters[i], n, style)
		if len(cell) > maxCellChars {
			if r := []rune(cell); len(r) > maxCellChars {
				cell = string(r[:maxCellChars])
			}
		}
		xml.EscapeText(b, []byte(cell))
		b.WriteString(`</t></is></c>`)
	}
	b.WriteString(`</row>`)
}

// columnLetters: 0 -> A, 25 -> Z, 26 -> AA.
func columnLetters(i int) string {
	s := ""
	for i++; i > 0; i = (i - 1) / 26 {
		s = string(rune('A'+(i-1)%26)) + s
	}
	return s
}
