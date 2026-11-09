package flow

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ProjectFile is the file that lists a project's models. It's looked for in
// the flow's folder and every folder above it.
const ProjectFile = "hunch.yaml"

// Project is a hunch.yaml: the models flows in this folder can use, and
// which one decides and writes unless told otherwise.
//
//	default: jev
//	writer: claude
//	models:
//	  jev:    {url: https://api.typesafe.ai/v1/systemone, key: tsk-...}
//	  laya:   http://localhost:8000/v1/systemone
//	  claude: {url: https://api.anthropic.com/v1/messages, key: sk-ant-..., model: claude-opus-5-5}
//
// A model is a URL; its path says which API it speaks. key and model are
// only needed where the server wants them; a key written as $NAME is read
// from the environment. mock is always there, answering offline.
type Project struct {
	Path         string
	Default      string
	DefaultGiven bool // default: is written, not the first model
	Writer       string
	Models       map[string]BackendConfig
	Order        []string // model names in file order
}

// ProjectPath is the hunch.yaml in dir or the nearest folder above it, or
// "" when there's none.
func ProjectPath(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		path := filepath.Join(dir, ProjectFile)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// FindProject reads the hunch.yaml in dir or the nearest folder above it,
// or returns nil when there's none.
func FindProject(dir string) (*Project, error) {
	path, err := ProjectPath(dir)
	if err != nil || path == "" {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := ParseProject(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p.Path = path
	return p, nil
}

func ParseProject(data []byte) (*Project, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	p := &Project{Models: map[string]BackendConfig{}}
	var fields map[string]*yaml.Node
	var keys []string
	if len(doc.Content) > 0 {
		var err error
		if fields, keys, err = mapping(doc.Content[0]); err != nil {
			return nil, err
		}
	}
	for _, k := range keys {
		v := fields[k]
		switch k {
		case "default":
			p.Default, p.DefaultGiven = v.Value, true
		case "writer":
			p.Writer = v.Value
		case "models":
			mfields, mkeys, err := mapping(v)
			if err != nil {
				return nil, errAt(v, "models: %v", err)
			}
			for _, name := range mkeys {
				cfg, err := modelConfig(name, mfields[name])
				if err != nil {
					return nil, errAt(mfields[name], "model %s: %v", name, err)
				}
				p.Models[name] = cfg
				p.Order = append(p.Order, name)
			}
		default:
			return nil, errAt(v, "unknown field %q (want default, writer, models)", k)
		}
	}
	if _, ok := p.Models["mock"]; !ok {
		p.Models["mock"] = BackendConfig{Name: "mock", Kind: "mock", Options: map[string]any{}}
		p.Order = append(p.Order, "mock")
	}
	for _, name := range []string{p.Default, p.Writer} {
		if _, ok := p.Models[name]; name != "" && !ok {
			return nil, fmt.Errorf("%q is not one of the models", name)
		}
	}
	if p.Default == "" {
		p.Default = p.Order[0]
	}
	return p, nil
}

var bedrockHost = regexp.MustCompile(`^bedrock-runtime\.([a-z0-9-]+)\.amazonaws\.com$`)

// modelConfig reads one model: a URL, or {url, key, model, ...}. The URL's
// path says which API it speaks; other fields (timeout, max_tokens,
// prices, a mock's answers) pass through to the backend.
func modelConfig(name string, n *yaml.Node) (BackendConfig, error) {
	opts := map[string]any{}
	if n.Kind == yaml.ScalarNode {
		opts["url"] = n.Value
	} else if err := n.Decode(&opts); err != nil {
		return BackendConfig{}, err
	}
	cfg := BackendConfig{Name: name, Options: opts}
	raw, _ := opts["url"].(string)
	delete(opts, "url")
	if k, ok := opts["key"].(string); ok {
		delete(opts, "key")
		// $NAME is read when the model is used, so a missing variable
		// only stops the models that need it.
		if env, isRef := strings.CutPrefix(k, "$"); isRef {
			if env == "" {
				return cfg, fmt.Errorf("key: $ needs a variable name, e.g. key: $LAYA_API_KEY")
			}
			opts["api_key_env"] = env
		} else {
			opts["api_key"] = k
		}
	}
	if raw == "" || raw == "mock" {
		if name != "mock" && raw != "mock" {
			return cfg, fmt.Errorf("needs a url (the endpoint, e.g. https://api.typesafe.ai/v1/systemone)")
		}
		cfg.Kind = "mock"
		return cfg, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return cfg, fmt.Errorf("url %q: want http(s)://host/path", raw)
	}
	path := strings.TrimSuffix(u.Path, "/")
	switch {
	case bedrockHost.MatchString(u.Host):
		cfg.Kind = "bedrock"
		opts["region"] = bedrockHost.FindStringSubmatch(u.Host)[1]
	case strings.HasSuffix(path, "/chat/completions"):
		cfg.Kind = "openai"
		opts["endpoint"] = raw
		if strings.HasSuffix(u.Host, ".openai.azure.com") || strings.HasSuffix(u.Host, ".cognitiveservices.azure.com") {
			opts["key_header"] = "api-key"
		}
	case strings.HasSuffix(path, "/v1/messages"):
		cfg.Kind = "anthropic"
		base := *u
		base.Path = strings.TrimSuffix(path, "/v1/messages") + "/"
		opts["base_url"] = base.String()
	case strings.HasSuffix(path, "/systemone") || strings.HasSuffix(path, "/decisions"):
		cfg.Kind = "jev"
		opts["url"] = raw
	default:
		return cfg, fmt.Errorf("can't tell what %s speaks from its path; give the endpoint: "+
			".../v1/systemone or .../decisions (decision models like Jev and Laya), "+
			".../chat/completions (OpenAI-style chat: OpenAI, Ollama, OpenRouter...), "+
			".../v1/messages (Anthropic), or https://bedrock-runtime.REGION.amazonaws.com", raw)
	}
	cfg.Options["endpoint_url"] = raw
	return cfg, nil
}

// UseProject gives the flow the project's models. Without a project only
// mock is there.
func (f *Flow) UseProject(p *Project) {
	if p == nil {
		p = &Project{Default: "mock", Models: map[string]BackendConfig{
			"mock": {Name: "mock", Kind: "mock", Options: map[string]any{}}}, Order: []string{"mock"}}
	}
	f.Project = p
	f.Backends = p.Models
	f.DefaultBackend = p.Default
	f.WriterBackend = p.Writer
}

// ProjectHelp says how to start a hunch.yaml, for when there's none.
func ProjectHelp(flowPath string) string {
	return fmt.Sprintf(`no %s in %s or any folder above it.

It lists the models flows can use. Create one in your project folder, e.g.:

  default: jev
  models:
    jev:  {url: https://api.typesafe.ai/v1/systemone, key: tsk-...}
    laya: http://localhost:8000/v1/systemone

Keep it out of git: it holds keys. Or try a flow offline with --model mock
(made-up answers).`, ProjectFile, filepath.Dir(flowPath))
}

// ParseWithProject parses a flow and gives it the models in a hunch.yaml's
// text, as Load does with the file next to it.
func ParseWithProject(flowSrc, projectSrc []byte) (*Flow, error) {
	f, err := Parse(flowSrc)
	if err != nil {
		return nil, err
	}
	p, err := ParseProject(projectSrc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ProjectFile, err)
	}
	f.UseProject(p)
	return f, nil
}
