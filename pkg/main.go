package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/crossplane-contrib/x-generation/pkg/generator"
	t "github.com/crossplane-contrib/x-generation/pkg/types"
	crossplanev1 "github.com/crossplane/crossplane/apis/apiextensions/v1"
	"github.com/ghodss/yaml"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-jsonnet"
	getter "github.com/hashicorp/go-getter"
	"github.com/pkg/errors"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

const (
	autogenHeader = "## WARNING: This file was autogenerated!\n" +
		"## Manual modifications will be overwritten\n" +
		"## unless ignore: true is set in generate.yaml!\n" +
		"## Last Modification: %s.\n" +
		"\n"
	baseURL = "https://raw.githubusercontent.com/crossplane-contrib/"
)

var globalLabels []string = []string{"crossplane.io/claim-name", "crossplane.io/claim-namespace", "crossplane.io/composite", "external-name"}

const (
	replaceGlobal t.GlobalHandlingType = "replace"
	appendGlobal  t.GlobalHandlingType = "append"
)

type Generator struct {
	Group                   string                   `yaml:"group" json:"group"`
	Name                    string                   `yaml:"name" json:"name"`
	Plural                  *string                  `yaml:"plural,omitempty" json:"plural,omitempty"`
	Version                 string                   `yaml:"version" json:"version"`
	ScriptFileName          *string                  `yaml:"scriptFile,omitempty"`
	ConnectionSecretKeys    *[]string                `yaml:"connectionSecretKeys,omitempty" json:"connectionSecretKeys,omitempty"`
	Ignore                  bool                     `yaml:"ignore"`
	PatchExternalName       *bool                    `yaml:"patchExternalName,omitempty" json:"patchExternalName,omitempty"`
	PatchlName              *bool                    `yaml:"patchName,omitempty" json:"patchName,omitempty"`
	ResourceName            *string                  `yaml:"resourceName,omitempty" json:"resourceName,omitempty"`
	UIDFieldPath            *string                  `yaml:"uidFieldPath,omitempty" json:"uidFieldPath,omitempty"`
	OverrideFields          []t.OverrideField        `yaml:"overrideFields" json:"overrideFields"`
	Compositions            []t.Composition          `yaml:"compositions" json:"compositions"`
	Tags                    t.LocalTagConfig         `yaml:"tags,omitempty" json:"tags,omitempty"`
	Labels                  t.LocalLabelConfig       `yaml:"labels,omitempty" json:"labels,omitempty"`
	Provider                t.ProviderConfig         `yaml:"provider" json:"provider"`
	ReadinessChecks         *bool                    `yaml:"readinessChecks,omitempty" json:"readinessChecks,omitempty"`
	OverrideFieldsInClaim   []t.OverrideFieldInClaim `yaml:"overrideFieldsInClaim" json:"overrideFieldsInClaim"`
	ExpandCompositionName   *bool                    `yaml:"expandCompositionName,omitempty" json:"expandCompositionName,omitempty"`
	AdditionalPipelineSteps []t.PipelineStep         `yaml:"additionalPipelineSteps,omitempty" json:"additionalPipelineSteps,omitempty"`
	TagType                 *string                  `yaml:"tagType,omitempty" json:"tagType,omitempty"`
	TagProperty             *string                  `yaml:"tagProperty,omitempty" json:"tagProperty,omitempty"`
	UsePipeline             *bool                    `yaml:"usePipeline,omitempty" json:"usePipeline,omitempty"`

	crd        extv1.CustomResourceDefinition
	crdSource  string
	configPath string
}

type jsonnetOutput map[string]interface{}

func (g *Generator) LoadConfig(path string) *Generator {
	g.configPath = filepath.Dir(path)
	y, err := os.ReadFile(path)
	if err != nil {
		log.Printf("Error loading generator: %+v\n", err)
	}
	err = yaml.Unmarshal(y, g)
	if err != nil {
		fmt.Printf("Error unmarshaling generator config: %v\n", err)
	}
	return g
}

func (g *Generator) LoadCRD(generatorConfig *t.GeneratorConfig) error {
	crdTempDir, err := os.MkdirTemp("", "gencrd")
	if err != nil {
		return errors.Errorf("Error creating CRD temp dir: %v\n", err)
	}

	defer os.RemoveAll(crdTempDir)

	crdFileName := filepath.Base(g.Provider.CRD.File)
	crdTempFile := filepath.Join(crdTempDir, crdFileName)

	var crdUrl string
	usedBaseURL := baseURL
	if g.Provider.BaseURL != nil {
		usedBaseURL = *g.Provider.BaseURL
	} else if generatorConfig.Provider.BaseURL != nil {
		usedBaseURL = *generatorConfig.Provider.BaseURL
	}

	providerName := generatorConfig.Provider.Name
	if g.Provider.Name != "" {
		providerName = g.Provider.Name
	}
	providerVersion := generatorConfig.Provider.Version
	if g.Provider.Name != "" {
		providerVersion = g.Provider.Version
	}

	if providerName == "" {
		return errors.Errorf("No provider name given for crd: %v\n", g.Provider.CRD.File)
	}

	if providerVersion == "" {
		return errors.Errorf("No provider version given for crd: %v\n", g.Provider.CRD.File)
	}

	crdUrl = fmt.Sprintf(usedBaseURL, providerName, providerVersion, g.Provider.CRD.File)
	client := &getter.Client{
		Ctx: context.Background(),
		Src: crdUrl,
		Dst: crdTempFile,
	}

	log.Printf("Retrieving CRD file from %s\n", g.Provider.CRD.File)
	err = client.Get()
	if err != nil {
		return errors.Errorf("Get CRD: %v\n", err)
	}

	crd, err := os.ReadFile(crdTempFile)
	if err != nil {
		return errors.Errorf("Error reading from CRD tempfile: %v\n", err)
	}

	if len(crd) < 1 {
		return errors.Errorf("CRD %s appears to be empty!\n", g.Provider.CRD.File)
	}

	r, err := yaml.YAMLToJSON(crd)
	if err != nil {
		return errors.Errorf("Convert YAML to JSON: %v\n", err)
	}
	var crd2 extv1.CustomResourceDefinition
	err = json.Unmarshal(r, &crd2)
	if err != nil {
		return errors.Errorf("Unmarshal crd content: %v\n", err)
	}
	version := g.Provider.CRD.Version
	if version == "" {
		version = g.Version
	}
	if g.TagType == nil || g.TagProperty == nil {
		tagType, tagProperty := checkTagType(crd2, version)
		if g.TagType == nil {
			g.TagType = &tagType
		}
		if g.TagProperty == nil {
			g.TagProperty = &tagProperty
		}
	}
	if err != nil {
		return errors.Errorf("Convert CRD to JSON: %v\n", err)
	}
	g.crdSource = string(r)
	g.crd = crd2
	return nil

}

// Check if the CRD uses a array of key-value-pairs or an object for tags
func checkTagType(crd extv1.CustomResourceDefinition, version string) (string, string) {
	tags, tagProperty, err := tryToGetTags(crd, version)
	if err != nil {
		return "noTag", ""
	}
	if tags.Type == "array" {

		subType := tags.Items.Schema.Type
		if subType == "object" {
			properties := tags.Items.Schema.Properties

			_, ok := properties["key"]
			_, ok2 := properties["value"]
			if ok && ok2 {
				return "keyValueArray", tagProperty
			}
			_, ok3 := properties["tagKey"]
			_, ok4 := properties["tagValue"]
			if ok3 && ok4 {
				return "tagKeyTagValueArray", tagProperty
			}
		}
	}
	if tags.Type == "object" {
		if tags.AdditionalProperties.Schema.Type == "string" {
			return "tagObject", tagProperty
		}
	}

	return "noTag", ""
}

// try to load the tags property of the crd from the given object
func tryToGetTags(crd extv1.CustomResourceDefinition, version string) (*extv1.JSONSchemaProps, string, error) {
	if len(crd.Spec.Versions) > 0 {
		for _, schemaVersion := range crd.Spec.Versions {
			if schemaVersion.Name == version {
				if specs, ok := schemaVersion.Schema.OpenAPIV3Schema.Properties["spec"]; ok {
					if forProvider, ok := specs.Properties["forProvider"]; ok {
						if tags, ok := forProvider.Properties["tags"]; ok {
							return &tags, "spec.forProvider.tags", nil
						}
						if tagging, ok := forProvider.Properties["tagging"]; ok {
							if tagSet, ok := tagging.Properties["tagSet"]; ok {
								return &tagSet, "spec.forProvider.tagging.tagSet", nil
							}
						}
					}
				}
			}
		}
	}
	return nil, "", errors.New("Could not find tags")
}

func getTagListAsString(g *Generator) string {
	return getJsonStringFromList(&g.Tags.FromLabels)
}

func getCommonTagsAsString(g *Generator) string {
	if len(g.Tags.Common) > 0 {
		return getJsonStringFromMap(&g.Tags.Common)
	}
	return "{}"
}

func getLabelListAsString(g *Generator) string {
	return getJsonStringFromList(&g.Labels.FromCRD)
}

func getCommonLabelsString(g *Generator) string {
	if len(g.Labels.Common) > 0 {
		return getJsonStringFromMap(&g.Labels.Common)
	}
	return "{}"
}

// Append list b to list a, items from b that already exist in a are not appened
func appendLists(a *[]string, b *[]string) *[]string {
	list := []string{}

	list = append(list, *a...)
	for _, v := range *b {
		if !listHas(a, v) {
			list = append(list, v)
		}
	}
	return &list
}

// add the values of b in map a and return the new map, if a value is given in a and b,
// the value in map b is used
func appendStringMaps(a, b map[string]string) map[string]string {
	for k, v := range b {
		a[k] = v
	}

	return a
}

// generate a JSON array representation of the given list, if the list is empty, returns []
func getJsonStringFromList(list *[]string) string {
	if len(*list) > 0 {
		marshaledList, _ := json.Marshal(list)

		return string(marshaledList)
	}
	return "[]"
}

// generate a JSON object representation of the given map
func getJsonStringFromMap(list *map[string]string) string {
	marshaledMap, _ := json.Marshal(list)

	return string(marshaledMap)
}

func (g *Generator) Exec(generatorConfig *t.GeneratorConfig, scriptPath, scriptFileOverride, outputPath string) {
	outPath := g.configPath
	if outputPath != "" {
		outPath = outputPath
	}
	header := []byte(fmt.Sprintf(autogenHeader,
		time.Now().Format("15:04:05 on 01-02-2006"),
	))
	if (generatorConfig.UsePipeline == nil || !*generatorConfig.UsePipeline) && (g.UsePipeline == nil || !*g.UsePipeline) {
		var fl string
		if scriptFileOverride != "" {
			fl = filepath.Join(scriptPath, scriptFileOverride)
		} else {
			fl = filepath.Join(scriptPath, "generate.jsonnet")
			if g.ScriptFileName != nil {
				fl = filepath.Join(scriptPath, *g.ScriptFileName)
			}
		}

		vm := jsonnet.MakeVM()

		if g.ExpandCompositionName == nil {
			if generatorConfig.ExpandCompositionName != nil {
				g.ExpandCompositionName = generatorConfig.ExpandCompositionName
			} else {
				f := false
				g.ExpandCompositionName = &f
			}
		}

		j, err := json.Marshal(&g)
		// a := string(j)
		// if a != "" {
		// }
		if err != nil {
			fmt.Printf("Error creating jsonnet input: %s", err)
		}
		readinessChecks := "true"
		if g.ReadinessChecks != nil {
			if !*g.ReadinessChecks {
				readinessChecks = "false"
			}
		}
		vm.ExtVar("config", string(j))
		vm.ExtVar("crd", g.crdSource)
		vm.ExtVar("globalLabels", getJsonStringFromList(&globalLabels))

		vm.ExtVar("tagList", getTagListAsString(g))

		vm.ExtVar("commonTags", getCommonTagsAsString(g))
		vm.ExtVar("labelList", getLabelListAsString(g))
		vm.ExtVar("commonLabels", getCommonLabelsString(g))

		if g.TagType != nil {
			vm.ExtVar("tagType", *g.TagType)
		} else {
			vm.ExtVar("tagType", "")
		}
		if g.TagProperty != nil {
			tagPropertyPath := strings.Split(*g.TagProperty, ".")
			tagProperty := tagPropertyPath[len(tagPropertyPath)-1]
			vm.ExtVar("tagProperty", tagProperty)
		} else {
			vm.ExtVar("tagProperty", "")
		}
		vm.ExtVar("compositionIdentifier", generatorConfig.CompositionIdentifier)
		vm.ExtVar("readinessChecks", readinessChecks)

		r, err := vm.EvaluateFile(fl)
		if err != nil {
			fmt.Printf("Error applying function %s: %s", fl, err)
		}

		jso := make(jsonnetOutput)

		err = json.Unmarshal([]byte(r), &jso)
		if err != nil {
			fmt.Printf("Error decoding jsonnet output: %s", err)
		}

		// header := []byte{}

		for fn, fc := range jso {
			yo, err := yaml.Marshal(fc)

			// Override x-kubernetes-validations fields if OverrideFieldsInClaim is given
			if g.OverrideFieldsInClaim != nil && fn == "definition" {
				var xrd crossplanev1.CompositeResourceDefinition
				err := yaml.Unmarshal(yo, &xrd)
				if err != nil {
					fmt.Printf("Error unmarshalling xrd %v", err)
				} else {
					updated, err := g.updateKubernetesValidation(&xrd)
					if err != nil {
						fmt.Printf("Error updating x-kubernetes-validations: %v", err)
					}
					if updated {
						yo, err = yaml.Marshal(xrd)
						if err != nil {
							fmt.Printf("Error updating definition with new x-kubernetes-validations: %v", err)
						}
						err = yaml.Unmarshal(yo, &fc)
						if err != nil {
							fmt.Printf("Error unmarshalling object %v", err)
						}
					}
				}
			}
			if err != nil {
				fmt.Printf("Error converting %s to YAML: %v", fn, err)
			}
			fp := filepath.Join(outPath, fn) + ".yaml"

			// Check if file already exists
			if _, err := os.Stat(fp); err == nil {
				yi, err := os.ReadFile(fp)
				if err != nil {
					fmt.Printf("Error reading from existing output file: %v", err)
				}
				ec := map[string]interface{}{}
				if err := yaml.Unmarshal(yi, &ec); err != nil {
					fmt.Printf("Error unmarshaling existing output file: %v", err)
				}

				if cmp.Equal(fc, ec) {
					continue
				}
			}

			fc := append(header, yo...)
			err = os.WriteFile(fp, fc, 0644)
			if err != nil {
				fmt.Printf("Error writing Generated File %s: %v", fp, err)
			}
		}
	} else {
		g2 := generator.XGenerator{
			Group:                     g.Group,
			Name:                      g.Name,
			Plural:                    g.Plural,
			PatchExternalName:         g.PatchExternalName,
			PatchlName:                g.PatchlName,
			ConnectionSecretKeys:      g.ConnectionSecretKeys,
			Compositions:              g.Compositions,
			Version:                   g.Version,
			Crd:                       g.crd,
			Provider:                  g.Provider,
			OverrideFields:            g.OverrideFields,
			Labels:                    g.Labels,
			GlobalLabels:              globalLabels,
			GeneratorConfig:           *generatorConfig,
			ReadinessChecks:           g.ReadinessChecks,
			ResourceName:              g.ResourceName,
			UIDFieldPath:              g.UIDFieldPath,
			ExpandCompositionName:     generatorConfig.ExpandCompositionName,
			TagType:                   g.TagType,
			TagProperty:               g.TagProperty,
			AutoReadyFunction:         generatorConfig.AutoReadyFunction,
			OverrideFieldsInClaim:     g.OverrideFieldsInClaim,
			PatchAndTransfromFunction: generatorConfig.PatchAndTransfromFunction,
		}
		if g.AdditionalPipelineSteps != nil {
			g2.AdditionalPipelineSteps = g.AdditionalPipelineSteps
		} else {
			g2.AdditionalPipelineSteps = generatorConfig.AdditionalPipelineSteps
		}
		filename := filepath.Join(outPath, "definition") + ".yaml"
		xrd, err := g2.GenerateXRD()
		if err != nil {
			log.Fatalf("Error creating xrd: %v", err)
		}
		rawContent, err := json.Marshal(xrd.Spec.Versions[0].Schema.OpenAPIV3Schema.Object)
		if err != nil {
			fmt.Printf("Error marhalling object: %v", err)
		}
		xrd.Spec.Versions[0].Schema.OpenAPIV3Schema.Raw = rawContent
		_, err = g.updateKubernetesValidation(xrd)
		if err != nil {
			fmt.Printf("Error updating x-kubernetes-validations: %v", err)
		}
		xrd2 := map[string]interface{}{
			"apiVersion": xrd.APIVersion,
			"kind":       xrd.Kind,
			"metadata": map[string]interface{}{
				"name": xrd.ObjectMeta.Name,
			},
			"spec": xrd.Spec,
		}
		if len(xrd.ObjectMeta.Labels) > 0 {
			xrd2["metadata"].(map[string]interface{})["labels"] = xrd.ObjectMeta.Labels
		}
		if len(xrd.ObjectMeta.Annotations) > 0 {
			xrd2["metadata"].(map[string]interface{})["annotations"] = xrd.ObjectMeta.Annotations
		}
		fileContent, err := yaml.Marshal(xrd2)
		if err != nil {
			fmt.Printf("Error updating definition with new x-kubernetes-validations: %v", err)
		}
		content := append(header, fileContent...)
		err = os.WriteFile(filename, content, 0644)
		if err != nil {
			fmt.Printf("Error writing Generated File %s: %v", filename, err)
		}
		// filename = filepath.Join(outPath, "commposition") + ".yaml"
		compositions, err := g2.GenerateComposition()
		if err != nil {
			log.Fatalf("Error creating composition: %v", err)
		}
		for _, p := range compositions {

			filename = filepath.Join(outPath, "composition-"+p.Name) + ".yaml"
			compositionContent := map[string]interface{}{
				"apiVersion": p.Composition.APIVersion,
				"kind":       p.Composition.Kind,
				"metadata": map[string]interface{}{
					"name":   p.Composition.ObjectMeta.Name,
					"labels": p.Composition.ObjectMeta.Labels,
				},
				"spec": p.Composition.Spec,
			}
			if len(p.Composition.ObjectMeta.Labels) > 0 {
				compositionContent["metadata"].(map[string]interface{})["labels"] = p.Composition.ObjectMeta.Labels
			}
			if len(p.Composition.ObjectMeta.Annotations) > 0 {
				compositionContent["metadata"].(map[string]interface{})["annotations"] = p.Composition.ObjectMeta.Annotations
			}
			fileContent, err = yaml.Marshal(compositionContent)
			if err != nil {
				fmt.Printf("Error updating definition with new x-kubernetes-validations: %v", err)
			}
			content := append(header, fileContent...)
			err = os.WriteFile(filename, content, 0644)
			if err != nil {
				fmt.Printf("Error writing Generated File %s: %v", filename, err)
			}
		}
	}
}

func (g *Generator) updateKubernetesValidation(xrd *crossplanev1.CompositeResourceDefinition) (bool, error) {
	schemaRaw := xrd.Spec.Versions[0].Schema.OpenAPIV3Schema.Raw
	var schema map[string]interface{}
	err := json.Unmarshal(schemaRaw, &schema)
	if err != nil {
		return false, err
	}
	properties, ok := schema["properties"].(map[string]interface{})
	if !ok {
		return false, errors.New("no properties")
	}

	spec, ok := properties["spec"].(map[string]interface{})
	if !ok {
		return false, errors.New("no spec")
	}

	kubernetesValidations, ok := spec["x-kubernetes-validations"].([]interface{})

	if !ok {
		return false, nil
	}
	replaceMap := map[string]string{}
	replaceMessageMap := map[string]string{}
	ignoreList := []string{}

	for _, override := range g.OverrideFieldsInClaim {
		if override.ManagedPath != nil {
			// var updatedClaimPath, updatedManagedPath string
			updatedClaimPath := strings.Replace(override.ClaimPath, "spec", "self", 1)
			updatedManagedPath := strings.Replace(*override.ManagedPath, "spec", "self", 1)
			replaceMap[updatedManagedPath] = updatedClaimPath
			replaceMessageMap[*override.ManagedPath] = override.ClaimPath
		}
		if override.Ignore {
			ignoreList = append(ignoreList, strings.Replace(override.ClaimPath, "spec", "self", 1))
		}
	}
	validationMapArray := []map[string]interface{}{}
	for _, validation := range kubernetesValidations {
		validationMap := validation.(map[string]interface{})
		rule := validationMap["rule"].(string)
		ignore := false
		for _, i := range ignoreList {
			if strings.Contains(rule, i) {
				ignore = true
				break
			}
		}
		if !ignore {
			message := validationMap["message"].(string)
			for old, new := range replaceMap {
				rule = strings.Replace(rule, old, new, -1)
			}
			for old, new := range replaceMessageMap {
				message = strings.Replace(message, old, new, -1)
			}

			validationMap["rule"] = rule
			validationMap["message"] = message
			validationMapArray = append(validationMapArray, validationMap)
		}
	}
	spec["x-kubernetes-validations"] = validationMapArray
	properties["spec"] = spec
	schema["properties"] = properties

	newSchema, err := json.Marshal(schema)
	if err != nil {
		return false, err
	}
	xrd.Spec.Versions[0].Schema.OpenAPIV3Schema.Raw = newSchema

	return true, nil
}

// Checks that the config for a generator is valid
// The tags we patch from labels must exist in the configuration of the generator,
// in the global configuration, or in the list of global labels
func (g *Generator) CheckConfig(generatorConfig *t.GeneratorConfig) error {
	commonLables := generatorConfig.Labels.Common
	listOfErrFields := []string{}

	for _, t := range g.Tags.FromLabels {
		if _, ok := commonLables[t]; !ok && !listHas(&g.Labels.FromCRD, t) && !listHas(&globalLabels, t) {
			listOfErrFields = append(listOfErrFields, t)
		}
	}
	if len(listOfErrFields) > 0 {
		return errors.New("Not all tags.fromLables entries exist in labels.fromCRD or global generator config or globalLabels: " + getJsonStringFromList(&listOfErrFields))
	}
	return nil
}

func (g *Generator) UpdateConfig(generatorConfig *t.GeneratorConfig) {
	if generatorConfig != nil {
		if g.Labels.GlobalHandling.FromCRD == appendGlobal {
			g.Labels.FromCRD = *appendLists(&generatorConfig.Labels.FromCRD, &g.Labels.FromCRD)
		} else if len(g.Labels.FromCRD) == 0 && g.Labels.GlobalHandling.FromCRD != replaceGlobal {
			g.Labels.FromCRD = generatorConfig.Labels.FromCRD
		}
		if g.Labels.GlobalHandling.Common == appendGlobal {
			g.Labels.Common = appendStringMaps(generatorConfig.Labels.Common, g.Labels.Common)
		} else if len(g.Labels.Common) == 0 && g.Labels.GlobalHandling.Common != replaceGlobal {
			g.Labels.Common = generatorConfig.Labels.Common
		}
		if g.Tags.GlobalHandling.FromLabels == appendGlobal {
			g.Tags.FromLabels = *appendLists(&generatorConfig.Tags.FromLabels, &g.Tags.FromLabels)
		} else if len(g.Tags.FromLabels) == 0 && g.Tags.GlobalHandling.FromLabels != replaceGlobal {
			g.Tags.FromLabels = generatorConfig.Tags.FromLabels
		}
		if g.Tags.GlobalHandling.Common == appendGlobal {
			g.Tags.Common = appendStringMaps(generatorConfig.Tags.Common, g.Tags.Common)
		} else if len(g.Tags.Common) == 0 && g.Tags.GlobalHandling.Common != replaceGlobal {
			g.Tags.Common = generatorConfig.Tags.Common
		}

	}
}

func parseArgs(configFile, generatorFile, inputPath, scriptFile, scriptPath, outputPath *string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	_, b, _, ok := runtime.Caller(0)
	if !ok {
		return errors.New("Unable to get generator module path")
	}
	sp := filepath.Join(filepath.Dir(b), "functions")

	flag.StringVar(generatorFile, "inputName", "generate.yaml", "input filename to search for in current directory")
	flag.StringVar(inputPath, "inputPath", cwd, "input filename to search for in current directory")
	flag.StringVar(scriptFile, "scriptName", "", "script filename to execute against input file(s) (default: generate.jsonnet or specified in each input file)")
	flag.StringVar(scriptPath, "scriptPath", sp, "path where script files are loaded from ")
	flag.StringVar(outputPath, "outputPath", "", "path where output files are created (default: same directory as input file)")
	flag.StringVar(configFile, "configFile", "./generator-config.yaml", "path where global config file can be found (default: ./generator-config.yaml)")

	flag.Parse()

	return nil
}

// Load the GeneratorConfig from the given path
func loadGeneratorConfig(path string) (*t.GeneratorConfig, error) {
	var generatorConfig t.GeneratorConfig
	y, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	err = yaml.Unmarshal(y, &generatorConfig)
	if err != nil {
		return nil, err
	}

	return &generatorConfig, nil
}

// Check if the given value exists in list
func listHas(list *[]string, value string) bool {
	for _, v := range *list {
		if v == value {
			return true
		}
	}
	return false
}

// Checks that the global configurator config is valid
// The tags we patch from labels must exist in the global configuration,
// or in the list of global labels
func checkConfig(generatorConfig *t.GeneratorConfig) error {
	if generatorConfig != nil {
		listOfErrFields := []string{}

		commonLables := generatorConfig.Labels.Common
		for _, t := range generatorConfig.Tags.FromLabels {
			if _, ok := commonLables[t]; !ok && !listHas(&generatorConfig.Labels.FromCRD, t) && !listHas(&globalLabels, t) {
				listOfErrFields = append(listOfErrFields, t)

			}
		}
		if len(listOfErrFields) > 0 {
			return errors.New("Not all tags.fromLables entries exist in labels.fromCRD or labels.Common or in globalLabels: " + getJsonStringFromList(&listOfErrFields))
		}
	}
	return nil
}

func main() {
	var configFile, generatorFile, inputPath, scriptFile, scriptPath, outputPath string

	if err := parseArgs(&configFile, &generatorFile, &inputPath, &scriptFile, &scriptPath, &outputPath); err != nil {
		fmt.Printf("Error parsing arguments: %s", err)
	}

	list := []string{}

	err := filepath.Walk(inputPath, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		if filepath.Base(path) == generatorFile {
			list = append(list, path)
		}
		return nil
	})

	if err != nil {
		fmt.Printf("Error finding generator files: %s", err)
	}

	fmt.Println(configFile)
	generatorConfig, err := loadGeneratorConfig(configFile)
	if err != nil {
		fmt.Println("Could not find generator config file")
		os.Exit(1)
	}
	err = checkConfig(generatorConfig)
	if err != nil {
		fmt.Printf("Generator config not valid: %s\n", err)
		os.Exit(1)
	}

	for _, m := range list {
		g := (&Generator{
			OverrideFields:        []t.OverrideField{},
			Compositions:          []t.Composition{},
			OverrideFieldsInClaim: []t.OverrideFieldInClaim{},
		}).LoadConfig(m)
		if g.Ignore {
			fmt.Printf("Generator for %s asks to be ignored, skipping...\n", g.Name)
			continue
		}
		if err := g.LoadCRD(generatorConfig); err != nil {
			fmt.Printf("CRD config not valid, skiping this : %s\n", err)
			continue
		}

		g.UpdateConfig(generatorConfig)
		if err := g.CheckConfig(generatorConfig); err != nil {
			fmt.Printf("CRD config not valid, skiping this : %s\n", err)
			continue
		}

		g.Exec(generatorConfig, scriptPath, scriptFile, outputPath)
	}
}
