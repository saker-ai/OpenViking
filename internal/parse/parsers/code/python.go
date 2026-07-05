package code

import (
	python "github.com/smacker/go-tree-sitter/python"
)

func init() {
	(&CodeParser{
		Name: "python",
		Ext:  ".py",
		Lang: python.GetLanguage(),
	}).register()
}
