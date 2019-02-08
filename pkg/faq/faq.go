package faq

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/Azure/draft/pkg/linguist"

	"github.com/jzelinskie/faq/pkg/formats"
	"github.com/jzelinskie/faq/pkg/jq"
	"github.com/sirupsen/logrus"
)

var yamlSeperator = "---"

// ProcessEachFile takes a list of files, and for each, attempts to convert it
// to a JSON value and runs ExecuteProgram against each.
func ProcessEachFile(inputFormat string, files []File, program string, programArgs ProgramArguments, outputWriter io.Writer, outputFormat string, outputConf OutputConfig, rawOutput bool) error {
	for fileNum, file := range files {
		decoder, err := determineDecoder(inputFormat, file)
		if err != nil {
			return err
		}

		encoder, err := determineEncoder(outputFormat, decoder)
		if err != nil {
			return err
		}

		fileBytes, err := file.Contents()
		if err != nil {
			return err
		}

		if len(bytes.TrimSpace(fileBytes)) != 0 {
			if streamable, ok := decoder.(formats.Streamable); ok {
				decoder := streamable.NewDecoder(fileBytes)
				itemNum := 1
				var writeSeperator bool
				for {
					data, err := decoder.MarshalJSONBytes()
					if err == io.EOF {
						break
					}
					if writeSeperator {
						fmt.Fprintln(outputWriter, yamlSeperator)
					}
					if err != nil {
						return fmt.Errorf("failed to jsonify file at %s: `%s`", file.Path(), err)
					}

					logrus.Debugf("file: %s (item %d), jsonified:\n%s", file.Path(), fileNum, string(data))
					err = ExecuteProgram(&data, program, programArgs, outputWriter, encoder, outputConf, rawOutput)
					if err != nil {
						return err
					}
					// If the output format is yaml, then write a document separator
					// between each output besides the first, and last.
					if formats.ToName(encoder) == "yaml" {
						writeSeperator = true
					}
					itemNum++
				}
				if formats.ToName(encoder) == "yaml" && fileNum != (len(files)-1) {
					fmt.Fprintln(outputWriter, yamlSeperator)
				}
			} else {
				data, err := decoder.MarshalJSONBytes(fileBytes)
				if err != nil {
					return fmt.Errorf("failed to jsonify file at %s: `%s`", file.Path(), err)
				}

				logrus.Debugf("file: %s, jsonified:\n%s", file.Path(), string(data))
				err = ExecuteProgram(&data, program, programArgs, outputWriter, encoder, outputConf, rawOutput)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// SlurpAllFiles takes a list of files, and for each, attempts to convert it to
// a JSON value and appends each JSON value to an array, and passes that array
// as the input ExecuteProgram.
func SlurpAllFiles(inputFormat string, files []File, program string, programArgs ProgramArguments, outputWriter io.Writer, encoder formats.Encoding, outputConf OutputConfig, rawOutput bool) error {
	data, err := combineJSONFilesToJSONArray(files, inputFormat)
	if err != nil {
		return err
	}

	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path())
	}
	logrus.Debugf("files: %q, jsonified:\n%s", paths, string(data))

	err = ExecuteProgram(&data, program, programArgs, outputWriter, encoder, outputConf, rawOutput)
	if err != nil {
		return err
	}

	return nil
}

// ExecuteProgram takes input, a single JSON value, and runs program via libjq
// against it, writing the results to outputWriter.
func ExecuteProgram(input *[]byte, program string, programArgs ProgramArguments, outputWriter io.Writer, encoder formats.Encoding, outputConf OutputConfig, rawOutput bool) error {
	if input == nil {
		input = new([]byte)
		*input = []byte("null")
	}

	args, err := marshalJqArgs(*input, programArgs)
	if err != nil {
		return err
	}

	outputs, err := jq.Exec(program, args, *input, rawOutput)
	if err != nil {
		return err
	}

	for i, output := range outputs {
		// for _, output := range outputs {
		var toWrite []byte
		if output != "" {
			var err error
			toWrite, err = encoder.UnmarshalJSONBytes([]byte(output))
			if err != nil {
				return fmt.Errorf("failed to encode jq program output as %s: %s", formats.ToName(encoder), err)
			}

			if outputConf.Pretty {
				toWrite, err = encoder.PrettyPrint(toWrite)
				if err != nil {
					return fmt.Errorf("failed to encode jq program output as pretty %s: %s", formats.ToName(encoder), err)
				}
			}
			if outputConf.Color {
				toWrite, err = encoder.Color(toWrite)
				if err != nil {
					return fmt.Errorf("failed to encode jq program output as color %s: %s", formats.ToName(encoder), err)
				}
			}
			toWrite = bytes.TrimSuffix(toWrite, []byte("\n"))
			// If the output format is yaml, then write a document separator
			// between each output besides the first, and last.
			if formats.ToName(encoder) == "yaml" && i != (len(outputs)-1) {
				fmt.Fprintln(outputWriter, yamlSeperator)
			}
		}

		fmt.Fprintln(outputWriter, string(toWrite))
	}

	return nil
}

func combineJSONFilesToJSONArray(files []File, inputFormat string) ([]byte, error) {
	var buf bytes.Buffer

	// append the first array bracket
	buf.WriteRune('[')

	// iterate over each file, appending it's contents to an array
	for i, file := range files {
		decoder, err := determineDecoder(inputFormat, file)
		if err != nil {
			return nil, err
		}

		fileBytes, err := file.Contents()
		if err != nil {
			return nil, err
		}

		if streamable, ok := decoder.(formats.Streamable); ok {
			decoder := streamable.NewDecoder(fileBytes)
			var dataList [][]byte
			for {
				data, err := decoder.MarshalJSONBytes()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, fmt.Errorf("failed to jsonify file at %s: `%s`", file.Path(), err)
				}
				if len(bytes.TrimSpace(data)) != 0 {
					dataList = append(dataList, data)
				}
			}
			// for each json value in dataList, write it, plus a comma after
			// it, as long it isn't the last item in dataList
			for j, data := range dataList {
				buf.Write(data)
				if j != len(dataList)-1 {
					buf.WriteRune(',')
				}
			}
			// append a comma between each file
			if len(dataList) != 0 && i != len(files)-1 {
				buf.WriteRune(',')
			}
		} else {
			data, err := decoder.MarshalJSONBytes(fileBytes)
			if err != nil {
				return nil, fmt.Errorf("failed to jsonify file at %s: `%s`", file.Path(), err)
			}
			if len(bytes.TrimSpace(data)) != 0 {
				buf.Write(data)
				if i != len(files)-1 {
					buf.WriteRune(',')
				}
			}
		}

	}
	// append the last array bracket
	buf.WriteRune(']')

	return buf.Bytes(), nil
}

// OutputConfig contains configuration for out to print out values
type OutputConfig struct {
	Pretty bool
	Color  bool
}

// ProgramArguments contains the arguments to a JQ program
type ProgramArguments struct {
	Args       []string
	Jsonargs   []interface{}
	Kwargs     map[string]string
	Jsonkwargs map[string]interface{}
}

func marshalJqArgs(jsonBytes []byte, jqArgs ProgramArguments) ([]byte, error) {
	var positionalArgsArray []interface{}
	programArgs := make(map[string]interface{})
	namedArgs := make(map[string]interface{})

	for _, value := range jqArgs.Args {
		positionalArgsArray = append(positionalArgsArray, value)
	}
	positionalArgsArray = append(positionalArgsArray, jqArgs.Jsonargs...)
	for key, value := range jqArgs.Kwargs {
		programArgs[key] = value
		namedArgs[key] = value
	}
	for key, value := range jqArgs.Jsonkwargs {
		programArgs[key] = value
		namedArgs[key] = value
	}

	programArgs["ARGS"] = map[string]interface{}{
		"positional": positionalArgsArray,
		"named":      namedArgs,
	}

	return json.Marshal(programArgs)
}

func determineDecoder(inputFormat string, file File) (formats.Encoding, error) {
	var decoder formats.Encoding
	var err error
	if inputFormat == "auto" {
		decoder, err = detectFormat(file)
	} else {
		var ok bool
		decoder, ok = formats.ByName(inputFormat)
		if !ok {
			err = fmt.Errorf("no supported format found named %s", inputFormat)
		}
	}
	if err != nil {
		return nil, err
	}

	return decoder, nil
}

func determineEncoder(outputFormat string, decoder formats.Encoding) (formats.Encoding, error) {
	var encoder formats.Encoding
	var ok bool
	if outputFormat == "auto" {
		encoder = decoder
	} else {
		encoder, ok = formats.ByName(outputFormat)
		if !ok {
			return nil, fmt.Errorf("no supported format found named %s", outputFormat)
		}
	}

	return encoder, nil
}

func detectFormat(file File) (formats.Encoding, error) {
	if ext := filepath.Ext(file.Path()); ext != "" {
		if format, ok := formats.ByName(ext[1:]); ok {
			return format, nil
		}
	}

	fileBytes, err := file.Contents()
	if err != nil {
		return nil, err
	}

	format := strings.ToLower(linguist.Analyse(fileBytes, linguist.LanguageHints(file.Path())))

	// If linguist doesn't detect care about then try to take a better guess.
	if _, ok := formats.ByName(format); !ok {
		// Look for either {, <, or --- at the beginning of the file to detect
		// json/xml/yaml.
		scanner := bufio.NewScanner(bytes.NewReader(fileBytes))
		keepScanning := true
		for keepScanning && scanner.Scan() {
			line := scanner.Bytes()
			for i, b := range line {
				// Go through each byte until we find a non-whitespace
				// character.
				if !unicode.IsSpace(rune(b)) {
					// If it's either character we're looking for, set the
					// correct format.
					if b == '{' {
						format = "json"
					} else if b == '<' {
						format = "xml"
					} else if b == '-' {
						// If we run into a -, then check if there is a yaml
						// document separator ---.
						if len(line[i:]) >= 3 && bytes.Equal(line[i:i+3], []byte(yamlSeperator)) {
							format = "yaml"
						}
					}
					// Break here because if the first non-whitespace character
					// isn't what we're looking for, then we didn't find what
					// we're looking for.
					keepScanning = false
					break
				}
			}
		}
	}

	// Go isn't smart enough to do this in one line.
	enc, ok := formats.ByName(format)
	if !ok {
		return nil, errors.New("failed to detect format of the input")
	}
	return enc, nil
}
