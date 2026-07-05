package code

import (
	"github.com/smacker/go-tree-sitter/php"
)

func init() {
	(&CodeParser{
		Name: "php",
		Ext:  ".php",
		Lang: php.GetLanguage(),
	}).register()
}
