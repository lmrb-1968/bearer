package custom

import (
	_ "embed"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/bearer/curio/pkg/detectors/custom/config"
	rubydetector "github.com/bearer/curio/pkg/detectors/ruby/custom_detector"
	sqldetector "github.com/bearer/curio/pkg/detectors/sql/custom_detector"
	"github.com/bearer/curio/pkg/detectors/types"
	"github.com/bearer/curio/pkg/parser"
	language "github.com/bearer/curio/pkg/parser/custom"
	parserdatatype "github.com/bearer/curio/pkg/parser/datatype"
	"github.com/bearer/curio/pkg/util/file"

	"github.com/bearer/curio/pkg/parser/nodeid"
	"github.com/bearer/curio/pkg/parser/sitter/sql"
	"github.com/bearer/curio/pkg/report"
	"github.com/bearer/curio/pkg/report/detectors"
	"github.com/bearer/curio/pkg/report/schema"
	sitter "github.com/smacker/go-tree-sitter"

	"github.com/smacker/go-tree-sitter/ruby"
)

type Detector struct {
	idGenerator      nodeid.Generator
	paramIdGenerator nodeid.Generator
	compiledRules    []config.CompiledRule

	Ruby language.Detector
	Sql  language.Detector
}

func New(idGenerator nodeid.Generator) types.Detector {
	detector := &Detector{
		idGenerator:      idGenerator,
		paramIdGenerator: &nodeid.IntGenerator{Counter: 1},
	}

	detector.Ruby = &rubydetector.Detector{}
	detector.Sql = &sqldetector.Detector{}

	return detector
}

func (detector *Detector) AcceptDir(dir *file.Path) (bool, error) {
	return true, nil
}

func (detector *Detector) CompileRules(rulesConfig config.Config) error {
	compiledRules := make([]config.CompiledRule, 0)

	for ruleName, rule := range rulesConfig.Rules {
		if rule.Disabled {
			continue
		}
		for _, lang := range rule.Languages {
			for _, pattern := range rule.Patterns {
				compiledRule, err := detector.compileRule(pattern, lang, detector.paramIdGenerator)
				if err != nil {
					return err
				}

				compiledRule.RuleName = ruleName
				compiledRule.Metavars = rule.Metavars
				compiledRule.ParamParenting = rule.ParamParenting
				compiledRules = append(compiledRules, compiledRule)
			}
		}
	}

	sort.Slice(compiledRules, func(i, j int) bool {
		return strings.Compare(compiledRules[i].RuleName, compiledRules[j].RuleName) == -1
	})

	detector.compiledRules = compiledRules

	return nil
}

func (detector *Detector) ProcessFile(file *file.FileInfo, dir *file.Path, report report.Report) (bool, error) {
	for _, rule := range detector.compiledRules {
		err := detector.executeRule(rule, file, report, detector.idGenerator)
		if err != nil {
			return false, err
		}
	}

	return false, nil
}

func (detector *Detector) compileRule(pattern string, lang string, idGenerator nodeid.Generator) (config.CompiledRule, error) {
	switch lang {
	case "ruby":
		return detector.Ruby.CompilePattern(pattern, detector.paramIdGenerator)
	case "sql":
		return detector.Sql.CompilePattern(pattern, detector.paramIdGenerator)
	}
	return config.CompiledRule{}, errors.New("unsupported language")
}

func languageMatchesFile(file *file.FileInfo, ruleLanguage string) bool {
	if file.Language == "Ruby" && ruleLanguage == "ruby" {
		return true
	}
	if ruleLanguage == "sql" {
		if file.Language != "SQL" &&
			// postgress
			file.Language != "PLpgSQL" && file.Language != "PLSQL" && file.Language != "SQLPL" &&
			// microsoft sql
			file.Language != "TSQL" {
			return false
		}
		return true
	}
	return false
}

func (detector *Detector) executeRule(rule config.CompiledRule, file *file.FileInfo, report report.Report, idGenerator nodeid.Generator) error {
	for _, lang := range rule.Languages {
		if !languageMatchesFile(file, lang) {
			continue
		}

		sitterLang := getLanguage(lang)
		tree, err := parser.ParseFile(file, file.Path, sitterLang)
		if err != nil {
			return err
		}

		query := parser.QueryMustCompile(sitterLang, rule.Tree)

		captures := tree.QueryConventional(query)

		filteredCaptures, err := filterCaptures(rule.Params, captures)
		if err != nil {
			return err
		}

		err = detector.extractData(filteredCaptures, rule, report, lang, idGenerator)
		if err != nil {
			return err
		}

		tree.Close()
	}
	return nil
}

func (detector *Detector) extractData(captures []parser.Captures, rule config.CompiledRule, report report.Report, lang string, idGenerator nodeid.Generator) error {
	for _, capture := range captures {
		forExport := make(map[parser.NodeID]*parserdatatype.DataType)
		var parent *parserdatatype.DataType

		for _, param := range rule.Params {
			var paramTypes map[parser.NodeID]*parserdatatype.DataType
			var err error

			if param.ArgumentsExtract || param.ClassNameExtract {
				paramTypes, err = detector.extractArguments(lang, capture[param.BuildFullName()], idGenerator)
				if err != nil {
					return err
				}

				if rule.ParamParenting {
					// join it as children to master param and export it
					if len(forExport) == 1 {
						for _, datatype := range paramTypes {
							parent.Properties[datatype.Name] = datatype
						}

						continue
					}

					// set parent
					if len(paramTypes) == 1 {
						for dataTypeID, datatype := range paramTypes {
							parent, err = parserdatatype.DeepestSingleChild(datatype)
							if err != nil {
								return err
							}

							forExport[dataTypeID] = datatype
							break
						}
						continue
					}
				}

				// set all for export
				for datatypeID, datatype := range paramTypes {
					forExport[datatypeID] = datatype
				}

			}

			if param.StringExtract {
				for _, metavar := range rule.Metavars {
					if metavar.Input == param.PatternName {
						matchNode := capture[param.BuildFullName()]
						content := matchNode.Content()
						searchRegexp := regexp.MustCompile(metavar.Regex)

						matches := searchRegexp.FindAllStringSubmatch(content, -1)

						for _, subgroupMatch := range matches {
							match := subgroupMatch[metavar.Output]

							matchType := &parserdatatype.DataType{
								Node:       matchNode,
								Name:       string(match),
								Type:       schema.SimpleTypeUknown,
								Properties: make(map[string]*parserdatatype.DataType),
							}
							matchNodeID := matchNode.ID()

							if rule.ParamParenting {
								parent.Properties[matchType.Name] = matchType
								continue
							}

							forExport[matchNodeID] = matchType

						}
					}
				}
			}
		}

		reportWrapper := WrapReport(report, rule.RuleName)
		parserdatatype.NewCompleteExport(reportWrapper, detectors.DetectorCustom, idGenerator, forExport)
	}
	return nil
}

func filterCaptures(params []config.Param, captures []parser.Captures) (filtered []parser.Captures, err error) {
	for _, capture := range captures {

		// filter captures not matching rules
		shouldIgnore := false
		for _, param := range params {
			if param.StringMatch != "" {
				if capture[param.BuildFullName()].Content() != param.StringMatch {
					shouldIgnore = true
					break
				}
			}

			if param.RegexMatch != "" {
				// todo regex compilation must happen as prestep
				regex, err := regexp.Compile(param.RegexMatch)
				if err != nil {
					return nil, err
				}

				paramContent := capture[param.BuildFullName()].Content()

				if !regex.Match([]byte(paramContent)) {
					shouldIgnore = true
					break
				}

			}
		}

		if shouldIgnore {
			continue
		}

		filtered = append(filtered, capture)
	}

	return filtered, err
}

func (detector *Detector) extractArguments(language string, node *parser.Node, idGenerator nodeid.Generator) (map[parser.NodeID]*parserdatatype.DataType, error) {
	switch language {
	case "ruby":
		return detector.Ruby.ExtractArguments(node, idGenerator)
	case "sql":
		return detector.Sql.ExtractArguments(node, idGenerator)
	}
	return nil, errors.New("unsupported language")
}

func getLanguage(input string) *sitter.Language {
	switch input {
	case "ruby":
		return ruby.GetLanguage()
	case "sql":
		return sql.GetLanguage()
	}
	return nil
}