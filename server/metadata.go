package server

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"smart-comfyui-gallery/config"
	"sort"
	"strconv"
	"strings"
)

var reLora = regexp.MustCompile(`(?i)<lora:([\w_\s.\-]+)(?::([\d.]+))*>`)
var reLyco = regexp.MustCompile(`(?i)<lyco:([\w_\s.\-]+):([\d.]+)>`)
var rePromptPunct = regexp.MustCompile(`[\\/\[\](){}]+`)
var rePromptClose = regexp.MustCompile(`>\s+`)
var reBracketSuffix = regexp.MustCompile(`\s*\[.*?\]$`)

func hsvToRGB(h, s, v float64) (int, int, int) {
	i := math.Floor(h * 6)
	f := h*6 - i
	p := v * (1 - s)
	q := v * (1 - f*s)
	t := v * (1 - (1-f)*s)
	var r, g, b float64
	switch int(i) % 6 {
	case 0:
		r, g, b = v, t, p
	case 1:
		r, g, b = q, v, p
	case 2:
		r, g, b = p, v, t
	case 3:
		r, g, b = p, q, v
	case 4:
		r, g, b = t, p, v
	case 5:
		r, g, b = v, p, q
	}
	return int(r * 255), int(g * 255), int(b * 255)
}

func nodeColor(nodeType string) string {
	h := sha1.Sum([]byte(nodeType + "a_salt_string"))
	n := int(h[0])<<8 | int(h[1])
	hue := float64(n%360) / 360.0
	r, g, b := hsvToRGB(hue, 0.7, 0.85)
	hexStr := make([]byte, 6)
	hex.Encode(hexStr[0:2], []byte{byte(r)})
	hex.Encode(hexStr[2:4], []byte{byte(g)})
	hex.Encode(hexStr[4:6], []byte{byte(b)})
	return "#" + string(hexStr)
}

var nodeCategoriesOrder = []string{"input", "model", "processing", "output", "others"}

var nodeCategories = map[string]string{
	"Load Checkpoint":        "input",
	"CheckpointLoaderSimple": "input",
	"Empty Latent Image":     "input",
	"CLIPTextEncode":         "input",
	"Load Image":             "input",
	"ModelMerger":            "model",
	"KSampler":               "processing",
	"KSamplerAdvanced":       "processing",
	"VAEDecode":              "processing",
	"VAEEncode":              "processing",
	"LatentUpscale":          "processing",
	"ConditioningCombine":    "processing",
	"PreviewImage":           "output",
	"SaveImage":              "output",
	"LoadImageOutput":        "input",
	"LoadImage":              "others",
	"VAELoader":              "others",
	"UNETLoader":             "others",
	"CLIPLoader":             "others",
	"Seed (rgthree)":         "others",
	"WanImageToVideo":        "others",
	"VHS_VideoCombine":       "others",
	"ModelSamplingSD3":       "others",
	"INTConstant":            "others",
	"SamplerCustom":          "others",
	"CheckpointLoader":       "others",
	"LoraLoader":             "others",
	"SaveAnimatedWEBP":       "output",
	"PreviewImageAdvanced":   "output",
	"LoadImageMask":          "input",
	"VHS_LoadVideo":          "input",
	"LoadAudio":              "input",
	"AudioLoader":            "input",
}

var nodeParamNames = map[string][]string{
	"CLIPTextEncode":         {"text"},
	"KSampler":               {"seed", "control_after_generate", "steps", "cfg", "sampler_name", "scheduler", "denoise"},
	"KSamplerAdvanced":       {"add_noise", "noise_seed", "control_after_generate", "steps", "cfg", "sampler_name", "scheduler", "start_at_step", "end_at_step", "return_with_leftover_noise"},
	"Load Checkpoint":        {"ckpt_name"},
	"CheckpointLoaderSimple": {"ckpt_name"},
	"Empty Latent Image":     {"width", "height", "batch_size"},
	"LatentUpscale":          {"upscale_method", "width", "height"},
	"SaveImage":              {"filename_prefix"},
	"ModelMerger":            {"ckpt_name1", "ckpt_name2", "ratio"},
	"Load Image":             {"image"},
	"LoadImageMask":          {"image"},
	"VHS_LoadVideo":          {"video"},
	"LoadAudio":              {"audio"},
	"AudioLoader":            {"audio"},
	"LoadImageOutput":        {"image"},
}

type nodeSummaryParam struct {
	Name        string      `json:"name"`
	Value       interface{} `json:"value"`
	IsInputFile bool        `json:"is_input_file"`
	InputURL    *string     `json:"input_url"`
}

type nodeSummaryItem struct {
	ID       interface{}        `json:"id"`
	Type     string             `json:"type"`
	Category string             `json:"category"`
	Color    string             `json:"color"`
	Params   []nodeSummaryParam `json:"params"`
}

func skipJSONValue(dec *json.Decoder, first json.Token) error {
	if d, ok := first.(json.Delim); ok {
		switch d {
		case '{':
			for dec.More() {
				if _, err := dec.Token(); err != nil {
					return err
				}
				tok, err := dec.Token()
				if err != nil {
					return err
				}
				if err := skipJSONValue(dec, tok); err != nil {
					return err
				}
			}
			_, err := dec.Token()
			return err
		case '[':
			for dec.More() {
				tok, err := dec.Token()
				if err != nil {
					return err
				}
				if err := skipJSONValue(dec, tok); err != nil {
					return err
				}
			}
			_, err := dec.Token()
			return err
		}
	}
	return nil
}

func nodeIDAndWidgetsValuesKeyOrder(nodeRaw []byte) (string, []string) {
	dec := json.NewDecoder(bytes.NewReader(nodeRaw))
	tok, err := dec.Token()
	if err != nil {
		return "", nil
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		return "", nil
	}
	idStr := ""
	var keys []string
	for dec.More() {
		kTok, err := dec.Token()
		if err != nil {
			return "", nil
		}
		k, ok := kTok.(string)
		if !ok {
			return "", nil
		}
		vTok, err := dec.Token()
		if err != nil {
			return "", nil
		}
		if k == "id" && idStr == "" {
			switch t := vTok.(type) {
			case string:
				idStr = t
			case float64:
				idStr = strconv.Itoa(int(t))
			case json.Number:
				idStr = t.String()
			default:
				idStr = fmt.Sprint(t)
			}
			continue
		}
		if k == "widgets_values" && keys == nil {
			if d2, ok2 := vTok.(json.Delim); ok2 && d2 == '{' {
				keys = []string{}
				for dec.More() {
					kk, err := dec.Token()
					if err != nil {
						return idStr, keys
					}
					ks, ok3 := kk.(string)
					if !ok3 {
						return idStr, keys
					}
					keys = append(keys, ks)
					vv, err := dec.Token()
					if err != nil {
						return idStr, keys
					}
					_ = skipJSONValue(dec, vv)
				}
				_, _ = dec.Token()
				continue
			}
			_ = skipJSONValue(dec, vTok)
			continue
		}
		_ = skipJSONValue(dec, vTok)
	}
	_, _ = dec.Token()
	if idStr == "" || len(keys) == 0 {
		return "", nil
	}
	return idStr, keys
}

func widgetsValuesKeyOrderByNodeID(workflowJSON string) map[string][]string {
	var wf struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(workflowJSON), &wf); err != nil || len(wf.Nodes) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, raw := range wf.Nodes {
		id, keys := nodeIDAndWidgetsValuesKeyOrder(raw)
		if id != "" && len(keys) > 0 {
			out[id] = keys
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func generateNodeSummary(workflowJSON string) []nodeSummaryItem {
	var raw any
	if err := json.Unmarshal([]byte(workflowJSON), &raw); err != nil {
		return []nodeSummaryItem{}
	}

	var nodes []map[string]any
	isAPI := false
	var widgetsOrder map[string][]string

	if m, ok := raw.(map[string]any); ok {
		if n, ok2 := m["nodes"].([]any); ok2 {
			widgetsOrder = widgetsValuesKeyOrderByNodeID(workflowJSON)
			for _, it := range n {
				if nm, ok3 := it.(map[string]any); ok3 {
					if mode, ok4 := nm["mode"].(float64); ok4 && int(mode) != 0 {
						continue
					}
					nodes = append(nodes, nm)
				}
			}
		} else {
			isAPI = true
			for nodeID, v := range m {
				nm, ok3 := v.(map[string]any)
				if !ok3 {
					continue
				}
				ct, ok4 := nm["class_type"].(string)
				if !ok4 || ct == "" {
					continue
				}
				entry := map[string]any{}
				for k, vv := range nm {
					entry[k] = vv
				}
				entry["id"] = nodeID
				entry["type"] = ct
				if inputs, ok5 := nm["inputs"].(map[string]any); ok5 {
					entry["inputs"] = inputs
				} else {
					entry["inputs"] = map[string]any{}
				}
				nodes = append(nodes, entry)
			}
		}
	}

	if len(nodes) == 0 {
		return []nodeSummaryItem{}
	}

	sort.Slice(nodes, func(i, j int) bool {
		ci := nodeCategories[nodes[i]["type"].(string)]
		cj := nodeCategories[nodes[j]["type"].(string)]
		if ci == "" {
			ci = "others"
		}
		if cj == "" {
			cj = "others"
		}
		oi := indexOf(nodeCategoriesOrder, ci)
		oj := indexOf(nodeCategoriesOrder, cj)
		if oi != oj {
			return oi < oj
		}
		return nodeIDAsInt(nodes[i]["id"]) < nodeIDAsInt(nodes[j]["id"])
	})

	validExt := map[string]bool{
		".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true, ".jfif": true, ".bmp": true, ".tiff": true,
		".mp4": true, ".mov": true, ".webm": true, ".mkv": true, ".avi": true,
		".mp3": true, ".wav": true, ".ogg": true, ".flac": true, ".m4a": true, ".aac": true,
	}

	baseInput := filepath.Clean(config.Cfg.BaseInputPath)
	baseInputAbs, _ := filepath.Abs(baseInput)

	out := []nodeSummaryItem{}
	for _, node := range nodes {
		nt, _ := node["type"].(string)
		params := []nodeSummaryParam{}
		appendParam := func(name string, v any) {
			display := v
			if lst, ok := v.([]any); ok {
				if len(lst) == 2 {
					if s0, ok2 := lst[0].(string); ok2 {
						display = fmt.Sprintf("(Link to %s)", s0)
					} else {
						display = fmt.Sprint(v)
					}
				} else {
					display = fmt.Sprint(v)
				}
			}

			isInput := false
			var inputURL *string

			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				clean := strings.ReplaceAll(strings.TrimSpace(s), "\\", "/")
				clean = reBracketSuffix.ReplaceAllString(clean, "")
				ext := strings.ToLower(filepath.Ext(clean))
				if validExt[ext] {
					filename := filepath.Base(clean)
					candidates := []string{
						filepath.Join(config.Cfg.BaseInputPath, clean),
						filepath.Join(config.Cfg.BaseInputPath, filename),
						filepath.Clean(filepath.Join(config.Cfg.BaseInputPath, clean)),
					}
					for _, cand := range candidates {
						if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
							absCand, _ := filepath.Abs(cand)
							if baseInputAbs != "" && strings.HasPrefix(absCand, baseInputAbs) {
								rel, _ := filepath.Rel(baseInputAbs, absCand)
								rel = strings.ReplaceAll(rel, "\\", "/")
								isInput = true
								url := "/galleryout/input_file/" + rel
								inputURL = &url
								display = clean
								break
							}
						}
					}
				}
			}

			params = append(params, nodeSummaryParam{Name: name, Value: display, IsInputFile: isInput, InputURL: inputURL})
		}

		if isAPI {
			if inputs, ok := node["inputs"].(map[string]any); ok {
				keys := make([]string, 0, len(inputs))
				for k := range inputs {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				for _, k := range keys {
					appendParam(k, inputs[k])
				}
			}
		} else {
			names := nodeParamNames[nt]
			switch wv := node["widgets_values"].(type) {
			case []any:
				for i, v := range wv {
					name := "param_" + strconv.Itoa(i+1)
					if i < len(names) {
						name = names[i]
					}
					appendParam(name, v)
				}
			case map[string]any:
				var keys []string
				if widgetsOrder != nil {
					if id, ok := node["id"].(float64); ok {
						keys = widgetsOrder[strconv.Itoa(int(id))]
					} else if id, ok := node["id"].(string); ok {
						keys = widgetsOrder[id]
					}
				}
				if len(keys) == 0 {
					keys = make([]string, 0, len(wv))
					for k := range wv {
						keys = append(keys, k)
					}
					sort.Strings(keys)
				}
				for i, k := range keys {
					name := "param_" + strconv.Itoa(i+1)
					if i < len(names) {
						name = names[i]
					}
					appendParam(name, k)
				}
			}
		}

		cat := nodeCategories[nt]
		if cat == "" {
			cat = "others"
		}
		out = append(out, nodeSummaryItem{
			ID:       node["id"],
			Type:     nt,
			Category: cat,
			Color:    nodeColor(nt),
			Params:   params,
		})
	}
	return out
}

func indexOf(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return len(xs)
}

func nodeIDAsInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case string:
		if i, err := strconv.Atoi(t); err == nil {
			return i
		}
		return 0
	default:
		return 0
	}
}

func cleanPromptText(x string) (string, []map[string]any) {
	if x == "" {
		return "", []map[string]any{}
	}
	x = strings.ReplaceAll(x, "，", ",")
	x = strings.ReplaceAll(x, "-", " ")
	x = strings.ReplaceAll(x, "_", " ")
	x = rePromptClose.ReplaceAllString(x, "> , ")
	x = strings.ReplaceAll(x, " BREAK ", " , BREAK , ")
	tagList := strings.Split(x, ",")
	finalTags := []string{}
	loras := []map[string]any{}
	for _, tag := range tagList {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if m := reLora.FindStringSubmatch(tag); len(m) > 0 {
			val := 1.0
			if len(m) > 2 && m[2] != "" {
				if f, err := strconv.ParseFloat(m[2], 64); err == nil {
					val = f
				}
			}
			loras = append(loras, map[string]any{"name": m[1], "value": val})
			continue
		}
		if m := reLyco.FindStringSubmatch(tag); len(m) > 0 {
			val := 1.0
			if len(m) > 2 && m[2] != "" {
				if f, err := strconv.ParseFloat(m[2], 64); err == nil {
					val = f
				}
			}
			loras = append(loras, map[string]any{"name": m[1], "value": val})
			continue
		}
		clean := strings.TrimSpace(rePromptPunct.ReplaceAllString(tag, ""))
		if clean != "" {
			finalTags = append(finalTags, clean)
		}
	}
	text := strings.Join(finalTags, ", ")
	return text, loras
}

func parseComfyMetadata(workflowJSON string) map[string]interface{} {
	meta := map[string]interface{}{
		"seed": nil, "steps": nil, "cfg": nil, "sampler": nil,
		"scheduler": nil, "model": nil, "positive_prompt": "",
		"negative_prompt": "", "positive_prompt_clean": "",
		"width": nil, "height": nil, "loras": []map[string]any{},
	}
	if strings.TrimSpace(workflowJSON) == "" {
		return meta
	}

	var raw any
	if err := json.Unmarshal([]byte(workflowJSON), &raw); err != nil {
		return meta
	}

	data := map[string]any{}
	switch t := raw.(type) {
	case []any:
		for i, n := range t {
			data[strconv.Itoa(i)] = n
		}
	case map[string]any:
		data = t
	default:
		return meta
	}

	samplerID := ""
	for id, n := range data {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		ct, _ := m["class_type"].(string)
		if strings.Contains(ct, "KSampler") || strings.Contains(ct, "SamplerCustom") {
			samplerID = id
			break
		}
	}
	if samplerID != "" {
		sNode, _ := data[samplerID].(map[string]any)
		inputs, _ := sNode["inputs"].(map[string]any)
		if v, ok := inputs["seed"]; ok {
			meta["seed"] = realValue(data, v)
		}
		if v, ok := inputs["steps"]; ok {
			meta["steps"] = realValue(data, v)
		}
		if v, ok := inputs["cfg"]; ok {
			meta["cfg"] = realValue(data, v)
		}
		if v, ok := inputs["sampler_name"]; ok {
			meta["sampler"] = realValue(data, v)
		} else if v, ok := inputs["sampler"]; ok {
			meta["sampler"] = realValue(data, v)
		}
		if v, ok := inputs["scheduler"]; ok {
			meta["scheduler"] = realValue(data, v)
		}

		if v, ok := inputs["positive"]; ok {
			meta["positive_prompt"] = promptFromLink(data, v)
		}
		if v, ok := inputs["negative"]; ok {
			meta["negative_prompt"] = promptFromLink(data, v)
		}

		if v, ok := inputs["model"]; ok {
			meta["model"] = modelFromLink(data, v)
		}
		if v, ok := inputs["latent_image"]; ok {
			w, h := sizeFromLink(data, v)
			if w != nil {
				meta["width"] = w
			}
			if h != nil {
				meta["height"] = h
			}
		}
	}

	if pp, ok := meta["positive_prompt"].(string); ok && pp != "" {
		clean, loras := cleanPromptText(pp)
		meta["positive_prompt_clean"] = clean
		meta["loras"] = loras
	}
	if meta["negative_prompt"] == meta["positive_prompt"] {
		meta["negative_prompt"] = ""
	}

	return meta
}

func isTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return strings.TrimSpace(t) != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	case uint64:
		return t != 0
	default:
		s := strings.TrimSpace(toString(v))
		return s != "" && s != "0" && s != "null" && s != "false"
	}
}

func isMetaSolid(meta map[string]interface{}) bool {
	prompt, _ := meta["positive_prompt"].(string)
	hasPrompt := len(strings.TrimSpace(prompt)) > 5
	tech := 0
	if isTruthy(meta["seed"]) {
		tech++
	}
	if isTruthy(meta["model"]) {
		tech++
	}
	if isTruthy(meta["steps"]) {
		tech++
	}
	if isTruthy(meta["sampler"]) {
		tech++
	}
	return hasPrompt && tech >= 2
}

func toString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		if t == math.Trunc(t) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

func isLinkValue(v any) (string, bool) {
	a, ok := v.([]any)
	if !ok || len(a) < 1 {
		return "", false
	}
	switch id := a[0].(type) {
	case string:
		return id, true
	case float64:
		return strconv.Itoa(int(id)), true
	default:
		return "", false
	}
}

func realValue(data map[string]any, v any) any {
	id, ok := isLinkValue(v)
	if !ok {
		return v
	}
	node, ok := data[id].(map[string]any)
	if !ok {
		return v
	}
	inputs, _ := node["inputs"].(map[string]any)
	for _, key := range []string{"value", "int", "float", "string", "text"} {
		if vv, ok := inputs[key]; ok {
			return realValue(data, vv)
		}
	}
	if wv, ok := node["widgets_values"].([]any); ok && len(wv) > 0 {
		return wv[0]
	}
	return v
}

func promptFromLink(data map[string]any, v any) string {
	id, ok := isLinkValue(v)
	if !ok {
		return ""
	}
	seen := map[string]bool{}
	var walk func(string) string
	walk = func(nodeID string) string {
		if seen[nodeID] {
			return ""
		}
		seen[nodeID] = true
		node, ok := data[nodeID].(map[string]any)
		if !ok {
			return ""
		}
		ct, _ := node["class_type"].(string)
		if strings.Contains(ct, "CLIPTextEncode") {
			inputs, _ := node["inputs"].(map[string]any)
			if raw, ok := inputs["text"]; ok {
				if s, ok2 := realValue(data, raw).(string); ok2 {
					return s
				}
			}
		}
		inputs, _ := node["inputs"].(map[string]any)
		if s, ok := inputs["text"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		if s, ok := inputs["t5xxl"].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
		if raw, ok := inputs["text"]; ok {
			childID, ok2 := isLinkValue(raw)
			if ok2 {
				if s := walk(childID); s != "" {
					return s
				}
			}
		}
		if raw, ok := inputs["conditioning"]; ok {
			childID, ok2 := isLinkValue(raw)
			if ok2 {
				if s := walk(childID); s != "" {
					return s
				}
			}
		}
		if wv, ok := node["widgets_values"].([]any); ok {
			for _, w := range wv {
				if s, ok2 := w.(string); ok2 && len(strings.TrimSpace(s)) > 5 {
					return s
				}
			}
		}
		return ""
	}
	return walk(id)
}

func modelFromLink(data map[string]any, v any) any {
	id, ok := isLinkValue(v)
	if !ok {
		return nil
	}
	node, ok := data[id].(map[string]any)
	if !ok {
		return nil
	}
	ct, _ := node["class_type"].(string)
	if strings.Contains(ct, "CheckpointLoader") {
		inputs, _ := node["inputs"].(map[string]any)
		if ck, ok := inputs["ckpt_name"]; ok {
			return realValue(data, ck)
		}
	}
	inputs, _ := node["inputs"].(map[string]any)
	for _, vv := range inputs {
		if m := modelFromLink(data, vv); m != nil {
			return m
		}
	}
	return nil
}

func sizeFromLink(data map[string]any, v any) (any, any) {
	id, ok := isLinkValue(v)
	if !ok {
		return nil, nil
	}
	node, ok := data[id].(map[string]any)
	if !ok {
		return nil, nil
	}
	ct, _ := node["class_type"].(string)
	if strings.Contains(ct, "EmptyLatentImage") {
		inputs, _ := node["inputs"].(map[string]any)
		w := realValue(data, inputs["width"])
		h := realValue(data, inputs["height"])
		return w, h
	}
	inputs, _ := node["inputs"].(map[string]any)
	for _, vv := range inputs {
		w, h := sizeFromLink(data, vv)
		if w != nil || h != nil {
			return w, h
		}
	}
	return nil, nil
}

func diffMetadata(a, b map[string]interface{}) []map[string]any {
	keys := []string{"model", "seed", "steps", "cfg", "sampler", "scheduler", "width", "height", "positive_prompt_clean", "negative_prompt"}
	items := []map[string]any{}
	for _, k := range keys {
		va := a[k]
		vb := b[k]
		da := toString(va)
		db := toString(vb)
		isDiff := da != db
		items = append(items, map[string]any{"key": k, "val_a": va, "val_b": vb, "is_diff": isDiff})
	}
	sort.SliceStable(items, func(i, j int) bool {
		di := items[i]["is_diff"].(bool)
		dj := items[j]["is_diff"].(bool)
		if di != dj {
			return di && !dj
		}
		return items[i]["key"].(string) < items[j]["key"].(string)
	})
	return items
}
