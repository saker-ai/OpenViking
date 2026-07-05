package parsers

import (
	"context"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/parse"
)

// ExcelParser parses .xlsx / .xlsm files using xuri/excelize/v2. Each
// sheet becomes one NodeSection; non-empty rows are joined into a
// Markdown table block.
type ExcelParser struct{}

func init() {
	parse.RegisterParser("excel", func() (parse.Parser, error) { return &ExcelParser{}, nil })
	parse.RegisterExtension(".xlsx", "excel")
	parse.RegisterExtension(".xlsm", "excel")
}

// SupportedExtensions returns .xlsx, .xlsm.
func (p *ExcelParser) SupportedExtensions() []string { return []string{".xlsx", ".xlsm"} }

// Parse opens the local xlsx file and emits one section per sheet.
func (p *ExcelParser) Parse(ctx context.Context, res *parse.LocalResource, opts parse.ParserOptions) (*parse.ParseResult, error) {
	if res == nil || res.Path == "" {
		return nil, domain.NewAppError(domain.CodeValidationFailed, 422,
			"excel parser: nil or empty local resource")
	}
	f, err := excelize.OpenFile(res.Path)
	if err != nil {
		return nil, domain.Wrap(domain.CodeParseFailed, 422, err)
	}
	defer f.Close()
	root := &parse.ResourceNode{Type: parse.NodeRoot, Title: titleFromPath(res.Path)}
	for _, sheet := range f.GetSheetList() {
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		section := &parse.ResourceNode{Type: parse.NodeSection, Level: 1, Title: sheet}
		var table [][]string
		for _, row := range rows {
			cells := make([]string, len(row))
			for i, c := range row {
				cells[i] = c
			}
			if !rowIsEmpty(cells) {
				table = append(table, cells)
			}
		}
		if len(table) > 0 {
			md := parse.FormatTableToMarkdown(table, true)
			cp, _ := writeTempText(md)
			section.AddChild(&parse.ResourceNode{
				Type:        parse.NodeTable,
				ContentPath: cp,
				Meta: map[string]any{
					"text":       md,
					"sheet_name": sheet,
					"row_count":  len(table),
				},
			})
		}
		root.AddChild(section)
	}
	pr := parse.NewParseResult(root, res.Path, "xlsx", "ExcelParser")
	return pr, nil
}

// ParseContent parses an in-memory xlsx blob.
func (p *ExcelParser) ParseContent(ctx context.Context, content []byte, sourcePath string, opts parse.ParserOptions) (*parse.ParseResult, error) {
	return nil, domain.Wrap(domain.CodeUnsupported, 501,
		fmt.Errorf("excel: ParseContent not supported (use Parse with a temp file)"))
}

// rowIsEmpty reports whether every cell in a row is empty/whitespace.
func rowIsEmpty(row []string) bool {
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
