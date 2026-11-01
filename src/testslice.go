package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// TEMPORARY: the small files for trying an import. To drop the feature, delete this file and testslice_test.go; in
// main.go the call in run(), result.TestReady, the test paragraph in summary(), testName and config.test(); and the
// `test` lines in README.md.

// minTestDeals: below this, production holds only a small update, too little to try an import with.
const minTestDeals = 5

// writeTestSlice rebuilds the test folder from the production files: about 10 recent deals, their buyers and their
// products, so everything in it links up when imported. It reads production and never changes it. made is true only
// when the folder now holds this data. When a file is missing or too few deals can be used, the folder is left as it
// was and made is false. The three files are replaced together, so the folder never holds a mix of old and new.
func writeTestSlice(production, test string) (made bool, err error) {
	products, err := readIfThere(filepath.Join(production, "products.csv"))
	if products == nil {
		return false, err
	}
	contacts, err := readIfThere(filepath.Join(production, "offline_contact.csv"))
	if contacts == nil {
		return false, err
	}
	deals, err := readIfThere(filepath.Join(production, "offline_deals.csv"))
	if deals == nil {
		return false, err
	}
	dealKey, dealName, dealProduct := slices.Index(deals.header, "Contact key"), slices.Index(deals.header, "Contact"), slices.Index(deals.header, "Product")
	contactKey, productName := slices.Index(contacts.header, "Contact key"), slices.Index(products.header, "Name")
	if dealKey < 0 || dealName < 0 || dealProduct < 0 || contactKey < 0 || productName < 0 || deals.header[0] != "ID" {
		return false, fmt.Errorf("the columns are not the expected ones")
	}

	known, catalog := map[string]bool{}, map[string]bool{}
	for _, row := range contacts.rows {
		known[row[contactKey]] = true
	}
	for _, row := range products.rows {
		catalog[row[productName]] = true
	}
	// A small update holds only what changed: a deal can name a contact or product that is not in it. Such a deal
	// would not link up when imported.
	linked := func(d [][]string) bool {
		if d[0][dealKey] == "" || !known[d[0][dealKey]] {
			return false
		}
		return !slices.ContainsFunc(d, func(row []string) bool { return row[dealProduct] != "" && !catalog[row[dealProduct]] })
	}
	var multi, single, shared [][][]string // deals, newest first: named buyer with many / one product, name left empty
	all := records(deals.rows, true)
	for i := len(all) - 1; i >= 0; i-- {
		d := all[i]
		name := d[0][dealName]
		switch {
		case !linked(d):
		case name == "":
			shared = append(shared, d)
		case len(d) >= 2:
			multi = append(multi, d)
		default:
			single = append(single, d)
		}
	}
	pick := pickDeals(multi, single, shared, dealKey)
	if len(pick) < minTestDeals {
		return false, nil
	}

	keys, names := map[string]bool{}, map[string]bool{}
	dealRows := [][]string{}
	title := slices.Index(deals.header, "Deal name")
	for i, d := range pick {
		keys[d[0][dealKey]] = true
		for _, row := range d {
			if row[dealProduct] != "" {
				names[row[dealProduct]] = true
			}
			row = slices.Clone(row)
			row[0] = strconv.Itoa(i + 1)
			if title >= 0 && row[title] != "" {
				row[title] = "TEST " + row[title] // so the test deals can be found and deleted in Bitrix
			}
			dealRows = append(dealRows, row)
		}
	}
	tmp := test + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return false, err
	}
	out := []struct {
		name string
		t    *table
		rows [][]string
	}{
		{"products.csv", products, keep(products.rows, productName, names)},
		{"offline_contact.csv", contacts, keep(contacts.rows, contactKey, keys)},
		{"offline_deals.csv", deals, dealRows},
	}
	for _, f := range out {
		t := *f.t
		t.rows = f.rows
		if err := writeCSV(filepath.Join(tmp, f.name), t); err != nil {
			return false, err
		}
	}
	return true, swapFolder(tmp, test)
}

// swapFolder puts tmp in place of dst. Windows refuses to rename a folder with a file open (in Excel, say): then the
// old folder simply stays as it was.
func swapFolder(tmp, dst string) error {
	old := dst + ".old"
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	hadOld := true
	if err := os.Rename(dst, old); os.IsNotExist(err) {
		hadOld = false
	} else if err != nil {
		return lockedError(dst, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		if hadOld {
			os.Rename(old, dst)
		}
		return lockedError(dst, err)
	}
	return os.RemoveAll(old)
}

// pickDeals covers the cases an import has to get right: 6 buyers with several products, one buyer with two deals,
// one more buyer with one product, and 2 deals whose Contact is empty because two people share the name.
func pickDeals(multi, single, shared [][][]string, key int) [][][]string {
	var pick [][][]string
	seen := map[string]bool{}
	for _, d := range multi {
		if len(pick) == 6 {
			break
		}
		if k := d[0][key]; !seen[k] {
			seen[k] = true
			pick = append(pick, d)
		}
	}
	var second, other [][]string
	for _, d := range single {
		if seen[d[0][key]] {
			if second == nil {
				second = d
			}
		} else if other == nil {
			other = d
		}
	}
	for _, d := range [][][]string{second, other} {
		if d != nil {
			pick = append(pick, d)
		}
	}
	return append(pick, shared[:min(2, len(shared))]...)
}

func keep(rows [][]string, column int, values map[string]bool) [][]string {
	out := [][]string{}
	for _, row := range rows {
		if values[row[column]] {
			out = append(out, row)
		}
	}
	return out
}

// readIfThere: nil and no error when the file does not exist.
func readIfThere(path string) (*table, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	t, err := readCSV(path)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return &t, nil
}
