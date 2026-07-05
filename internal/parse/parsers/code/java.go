package code

import (
	"github.com/smacker/go-tree-sitter/java"
)

func init() {
	(&CodeParser{
		Name: "java",
		Ext:  ".java",
		Lang: java.GetLanguage(),
	}).register()
}
