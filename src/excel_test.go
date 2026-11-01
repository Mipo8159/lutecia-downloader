package main

import (
	"archive/zip"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sheetXML(t *testing.T, path string) string {
	t.Helper()
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	for _, f := range z.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			r, _ := f.Open()
			defer r.Close()
			data, _ := io.ReadAll(r)
			return string(data)
		}
	}
	t.Fatal("no sheet")
	return ""
}

func TestExcelKeepsTextAsTextAndNumbersAsNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.xlsx")
	err := writeXLSX(path, table{
		header: []string{"Name", "Barcode", "Mobile", "Price", "Quantity"},
		rows: [][]string{
			{"ილია <&> \"x\"", "04800000000012", "0599123456", "12.50", "NaN"},
			{"", "", "", "", "3"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := sheetXML(t, path)
	if d := xml.NewDecoder(strings.NewReader(s)); true {
		for {
			if _, err := d.Token(); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("not valid XML: %v", err)
			}
		}
	}
	for _, want := range []string{
		`<c r="A1" t="inlineStr" s="1">`,
		"ილია &lt;&amp;&gt;",
		`<c r="B2" t="inlineStr"><is><t xml:space="preserve">04800000000012</t>`,
		`<c r="C2" t="inlineStr"><is><t xml:space="preserve">0599123456</t>`,
		`<c r="D2"><v>12.50</v></c>`,
		`<c r="E2" t="inlineStr"><is><t xml:space="preserve">NaN</t>`,
		`<c r="E3"><v>3</v></c>`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}

func TestExcelCellIsCutToWhatExcelAccepts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.xlsx")
	if err := writeXLSX(path, table{header: []string{"A"}, rows: [][]string{{strings.Repeat("ა", 40000)}}}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(sheetXML(t, path), "ა"); n != maxCellChars {
		t.Fatalf("got %d characters", n)
	}
}

func TestColumnLetters(t *testing.T) {
	for i, want := range map[int]string{0: "A", 25: "Z", 26: "AA", 27: "AB", 701: "ZZ", 702: "AAA"} {
		if got := columnLetters(i); got != want {
			t.Fatalf("%d: %s", i, got)
		}
	}
}

func TestExcelFolderFollowsProduction(t *testing.T) {
	fx := sliceFixture()
	f := &fake{ids: []string{"2026-10-14_030000"}, files: map[string]map[string]string{"2026-10-14_030000": {
		"products.csv": fx["products.csv"], "contacts.csv": fx["offline_contact.csv"], "deals.csv": fx["offline_deals.csv"],
	}}}
	cfg := testConfig(t, newFake(t, f).URL)
	res, err := run(cfg, noPull, io.Discard)
	if err != nil || !res.ExcelReady {
		t.Fatalf("%+v %v", res, err)
	}
	names := func() (out []string) {
		entries, _ := os.ReadDir(cfg.excel())
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}
	if got := strings.Join(names(), " "); got != "offline_contact.xlsx offline_deals.xlsx products.xlsx" {
		t.Fatal(got)
	}
	if msg := summary(cfg, res); !strings.Contains(msg, cfg.excel()) {
		t.Fatal(msg)
	}

	// an unchanged file is not rewritten, a changed one is
	copyPath, csvPath := filepath.Join(cfg.excel(), "products.xlsx"), filepath.Join(cfg.production(), "products.csv")
	before, _ := os.Stat(copyPath)
	if _, err := run(cfg, noPull, io.Discard); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.Stat(copyPath); !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("an unchanged file must not be rewritten")
	}
	os.WriteFile(csvPath, []byte(sliceProducts+"N,Changed,1\r\n"), 0o644)
	future := time.Now().Add(time.Hour)
	os.Chtimes(csvPath, future, future)
	if _, err := run(cfg, noPull, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sheetXML(t, copyPath), "Changed") {
		t.Fatal("a changed file must be copied again")
	}

	// the client imported and deleted production: customer data does not stay behind
	for _, n := range []string{"products.csv", "offline_contact.csv", "offline_deals.csv"} {
		os.Remove(filepath.Join(cfg.production(), n))
	}
	res, err = run(cfg, noPull, io.Discard)
	if err != nil || res.ExcelReady || len(names()) != 0 {
		t.Fatalf("%+v %v %v", res, err, names())
	}
}

func TestFailedExcelCopyHarmsNothing(t *testing.T) {
	fx := sliceFixture()
	f := &fake{ids: []string{"2026-10-14_030000"}, files: map[string]map[string]string{"2026-10-14_030000": {"products.csv": fx["products.csv"]}}}
	cfg := testConfig(t, newFake(t, f).URL)
	os.MkdirAll(cfg.Folder, 0o755)
	os.WriteFile(cfg.excel(), []byte("a file where the folder should go"), 0o644)
	var out strings.Builder
	res, err := run(cfg, noPull, &out)
	if err != nil || res.ExcelReady || !strings.Contains(out.String(), "Excel copies could be made") {
		t.Fatalf("%+v %v %q", res, err, out.String())
	}
	if read(t, filepath.Join(cfg.production(), "products.csv")) != fx["products.csv"] {
		t.Fatal("production must be intact")
	}
	if done, _ := readState(cfg.State); !done["2026-10-14_030000"] {
		t.Fatal("the bundle must stay on the downloaded list")
	}
}

func TestOnlyPlainNumbersBecomeNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.xlsx")
	rows := [][]string{{"1e5"}, {" 12"}, {"12,50"}, {"+5"}, {".5"}, {"Inf"}, {"-3.25"}, {"7"}}
	if err := writeXLSX(path, table{header: []string{"Price"}, rows: rows}); err != nil {
		t.Fatal(err)
	}
	if s := sheetXML(t, path); strings.Count(s, "<v>") != 2 || !strings.Contains(s, "<v>-3.25</v>") || !strings.Contains(s, "<v>7</v>") {
		t.Fatal(s)
	}
}

func TestEveryPartIsValidXMLAndControlCharactersAreSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.xlsx")
	if err := writeXLSX(path, table{header: []string{"A"}, rows: [][]string{{"bad\x01char\xff"}}}); err != nil {
		t.Fatal(err)
	}
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	for _, f := range z.File {
		r, _ := f.Open()
		d := xml.NewDecoder(r)
		for {
			if _, err := d.Token(); err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("%s: %v", f.Name, err)
			}
		}
		r.Close()
	}
}

func TestTooManyRowsForExcelIsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.xlsx")
	if err := writeXLSX(path, table{header: []string{"A"}, rows: make([][]string, maxSheetRows)}); err == nil {
		t.Fatal("want an error")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("no file must be made")
	}
}

func TestCleanupTouchesOnlyTheProgramsOwnCopies(t *testing.T) {
	dir := t.TempDir()
	prod, excel := filepath.Join(dir, "production"), filepath.Join(dir, "excel")
	writeFiles(t, prod, map[string]string{"products.csv": "x"})
	writeFiles(t, excel, map[string]string{"products.xlsx": "keep: its CSV is there", "offline_deals.xlsx": "goes", "my report.xlsx": "mine", "products": "mine"})
	writeFiles(t, filepath.Join(excel, "old"), map[string]string{"a.xlsx": "mine"})
	if stuck := cleanExcel(prod, excel); len(stuck) != 0 {
		t.Fatal(stuck)
	}
	if _, err := os.Stat(filepath.Join(excel, "offline_deals.xlsx")); err == nil {
		t.Fatal("the copy of a deleted file must go")
	}
	for _, name := range []string{"products.xlsx", "my report.xlsx", "products", "old/a.xlsx"} {
		if _, err := os.Stat(filepath.Join(excel, name)); err != nil {
			t.Fatalf("%s must stay: %v", name, err)
		}
	}
}

func TestCopiesGoEvenWhenTheServerCannotBeReached(t *testing.T) {
	cfg := testConfig(t, "http://127.0.0.1:1")
	writeFiles(t, cfg.excel(), map[string]string{"products.xlsx": "customer data"})
	if _, err := run(cfg, noPull, io.Discard); err == nil {
		t.Fatal("want a connection error")
	}
	if _, err := os.Stat(filepath.Join(cfg.excel(), "products.xlsx")); err == nil {
		t.Fatal("the copy must be gone")
	}
}

func TestChangedCopyAndRestoredOlderCSVAreBothRewritten(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "products.csv"), filepath.Join(dir, "products.xlsx")
	writeFiles(t, dir, map[string]string{"products.csv": sliceProducts + "A,First,1\r\n"})
	if err := syncOne(src, dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("edited by someone"), 0o644); err != nil { // newer than the CSV
		t.Fatal(err)
	}
	if err := syncOne(src, dst); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sheetXML(t, dst), "First") {
		t.Fatal("an edited copy must be replaced")
	}
	writeFiles(t, dir, map[string]string{"products.csv": sliceProducts + "A,Restored,1\r\n"})
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(src, old, old); err != nil {
		t.Fatal(err)
	}
	if err := syncOne(src, dst); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sheetXML(t, dst), "Restored") {
		t.Fatal("a restored older CSV must be copied")
	}
}

func TestOneCopyFailingDoesNotStopTheOthers(t *testing.T) {
	dir := t.TempDir()
	prod, excel := filepath.Join(dir, "production"), filepath.Join(dir, "excel")
	writeFiles(t, prod, map[string]string{"products.csv": sliceProducts + "A,B,1\r\n", "offline_contact.csv": sliceContacts + "N,C1\r\n"})
	writeFiles(t, filepath.Join(excel, "products.xlsx"), map[string]string{"blocks": "the rename"}) // like a locked file
	ready, err := syncExcel(prod, excel, []fileRows{{"products.csv", 1}, {"offline_contact.csv", 1}})
	if ready || err == nil || !strings.Contains(err.Error(), "products.xlsx may still show older data") {
		t.Fatal(ready, err)
	}
	if _, err := os.Stat(filepath.Join(excel, "offline_contact.xlsx")); err != nil {
		t.Fatal("the other copy must still be made")
	}
	if entries, _ := filepath.Glob(filepath.Join(excel, ".*.tmp")); len(entries) != 0 {
		t.Fatalf("no temporary files may stay: %v", entries)
	}
}
