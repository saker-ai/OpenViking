package code

import (
	"github.com/smacker/go-tree-sitter/rust"
)

func init() {
	(&CodeParser{
		Name: "rust",
		Ext:  ".rs",
		Lang: rust.GetLanguage(),
	}).register()
}
