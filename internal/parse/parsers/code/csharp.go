package code

import (
	"github.com/smacker/go-tree-sitter/csharp"
)

func init() {
	(&CodeParser{
		Name: "csharp",
		Ext:  ".cs",
		Lang: csharp.GetLanguage(),
	}).register()
}
