package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fileInfo is one file in the scanned repository.
type fileInfo struct {
	rel  string // slash-separated path relative to root
	size int64
	lang string // human language/name from extension, "" if unknown
}

// scanRepo walks root and returns matching files in stable sorted order,
// honoring the root .gitignore and skipping .git, dotfiles, and dot-dirs.
func scanRepo(root string) ([]fileInfo, error) {
	ignore := &gitignore{}
	if data, err := os.ReadFile(filepath.Join(root, ".gitignore")); err == nil {
		ignore = parseGitignore(strings.NewReader(string(data)))
	}

	var files []fileInfo
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ignore.Match(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, fileInfo{rel: rel, size: info.Size(), lang: langFor(rel)})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

// readTextFile reads a file's contents, returning ok=false if it is larger
// than maxBytes or looks binary — so content sampling never pulls in junk.
func readTextFile(root, rel string, maxBytes int64) ([]byte, bool) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil || info.Size() > maxBytes {
		return nil, false
	}
	data, err := os.ReadFile(full)
	if err != nil || isBinary(data) {
		return nil, false
	}
	return data, true
}

// langFor maps a file path to a display language name.
func langFor(path string) string {
	switch strings.ToLower(filepath.Base(path)) {
	case "dockerfile":
		return "Dockerfile"
	case "makefile":
		return "Makefile"
	case "go.mod", "go.sum":
		return "Go"
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "Go"
	case ".py":
		return "Python"
	case ".js", ".mjs", ".cjs":
		return "JavaScript"
	case ".ts":
		return "TypeScript"
	case ".tsx", ".jsx":
		return "React"
	case ".c", ".h":
		return "C"
	case ".cc", ".cpp", ".cxx", ".hpp":
		return "C++"
	case ".rs":
		return "Rust"
	case ".java":
		return "Java"
	case ".rb":
		return "Ruby"
	case ".php":
		return "PHP"
	case ".sh", ".bash":
		return "Shell"
	case ".sql":
		return "SQL"
	case ".json":
		return "JSON"
	case ".yaml", ".yml":
		return "YAML"
	case ".toml":
		return "TOML"
	case ".html":
		return "HTML"
	case ".css":
		return "CSS"
	case ".md", ".markdown":
		return "Markdown"
	default:
		return ""
	}
}

// fenceLang maps a display language to a Markdown code-fence hint.
func fenceLang(lang string) string {
	switch lang {
	case "Go":
		return "go"
	case "Python":
		return "python"
	case "JavaScript":
		return "javascript"
	case "TypeScript":
		return "typescript"
	case "React":
		return "tsx"
	case "C":
		return "c"
	case "C++":
		return "cpp"
	case "Rust":
		return "rust"
	case "Java":
		return "java"
	case "Ruby":
		return "ruby"
	case "PHP":
		return "php"
	case "Shell":
		return "bash"
	case "SQL":
		return "sql"
	case "JSON":
		return "json"
	case "YAML":
		return "yaml"
	case "TOML":
		return "toml"
	case "HTML":
		return "html"
	case "CSS":
		return "css"
	case "Markdown":
		return "markdown"
	case "Dockerfile":
		return "dockerfile"
	case "Makefile":
		return "makefile"
	default:
		return ""
	}
}
