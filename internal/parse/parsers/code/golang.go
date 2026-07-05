package code

import (
	"github.com/smacker/go-tree-sitter/golang"
)

func init() {
	(&CodeParser{
		Name: "golang",
		Ext:  ".go",
		Lang: golang.GetLanguage(),
	}).register()
}
