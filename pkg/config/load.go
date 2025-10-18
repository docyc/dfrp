// Copyright 2023 The frp Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"github.com/samber/lo"
	"gopkg.in/ini.v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/fatedier/frp/pkg/config/legacy"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/fatedier/frp/pkg/config/v1/validation"
	"github.com/fatedier/frp/pkg/msg"
	"github.com/fatedier/frp/pkg/util/util"
)

var (
	// 远程配置重试参数
	maxRetries    = 3
	retryInterval = 2 * time.Second
)

var glbEnvs map[string]string

func init() {
	glbEnvs = make(map[string]string)
	envs := os.Environ()
	for _, env := range envs {
		pair := strings.SplitN(env, "=", 2)
		if len(pair) != 2 {
			continue
		}
		glbEnvs[pair[0]] = pair[1]
	}
}

type Values struct {
	Envs map[string]string // environment vars
}

func GetValues() *Values {
	return &Values{
		Envs: glbEnvs,
	}
}

// fetchRemoteConfig 从HTTPS地址获取配置内容，带重试机制
func fetchRemoteConfig(url string) ([]byte, error) {
	// 仅允许HTTPS协议
	if !strings.HasPrefix(strings.ToLower(url), "https://") {
		return nil, fmt.Errorf("only HTTPS protocol is supported for remote configs")
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	var lastErr error
	for i := 0; i < maxRetries; i++ {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = fmt.Errorf("request failed: %w", err)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("remote server returned status: %s", resp.Status)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
			continue
		}

		content, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response: %w", err)
			if i < maxRetries-1 {
				time.Sleep(retryInterval)
			}
			continue
		}

		return content, nil
	}

	return nil, fmt.Errorf("failed after %d retries: %w", maxRetries, lastErr)
}

func DetectLegacyINIFormat(content []byte) bool {
	f, err := ini.Load(content)
	if err != nil {
		return false
	}
	if _, err := f.GetSection("common"); err == nil {
		return true
	}
	return false
}

func DetectLegacyINIFormatFromFile(path string) bool {
	b, err := LoadFileContentWithTemplate(path, GetValues())
	if err != nil {
		return false
	}
	return DetectLegacyINIFormat(b)
}

func RenderWithTemplate(in []byte, values *Values) ([]byte, error) {
	tmpl, err := template.New("frp").Funcs(template.FuncMap{
		"parseNumberRange":     parseNumberRange,
		"parseNumberRangePair": parseNumberRangePair,
	}).Parse(string(in))
	if err != nil {
		return nil, err
	}

	buffer := bytes.NewBufferString("")
	if err := tmpl.Execute(buffer, values); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// LoadFileContentWithTemplate 加载文件内容（支持本地文件和HTTPS远程URL）
func LoadFileContentWithTemplate(path string, values *Values) ([]byte, error) {
	var b []byte
	var err error

	// 区分远程URL和本地文件
	if strings.HasPrefix(strings.ToLower(path), "https://") {
		b, err = fetchRemoteConfig(path)
	} else {
		b, err = os.ReadFile(path)
	}

	if err != nil {
		return nil, err
	}
	return RenderWithTemplate(b, values)
}

func LoadConfigureFromFile(path string, c any, strict bool) error {
	content, err := LoadFileContentWithTemplate(path, GetValues())
	if err != nil {
		return err
	}
	return LoadConfigure(content, c, strict)
}

// DetectConfigFormat 仅通过内容检测配置文件格式（不依赖后缀）
func DetectConfigFormat(content []byte) string {
	// 清洗内容：去除前导空白字符，用于格式判断
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return "unknown"
	}

	// JSON特征：以{开头，以}结尾
	if (trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}') ||
		(trimmed[0] == '[' && trimmed[len(trimmed)-1] == ']') {
		var jsonTest interface{}
		if err := json.Unmarshal(trimmed, &jsonTest); err == nil {
			return "json"
		}
	}

	// INI特征：包含[section]格式的行
	lines := bytes.Split(trimmed, []byte("\n"))
	for _, line := range lines {
		l := bytes.TrimSpace(line)
		if len(l) >= 2 && l[0] == '[' && l[len(l)-1] == ']' {
			if _, err := ini.Load(content); err == nil {
				return "ini"
			}
			break
		}
	}

	// TOML特征：尝试解析为TOML（最后检测，因为TOML格式更灵活）
	var tomlTest interface{}
	if err := toml.Unmarshal(trimmed, &tomlTest); err == nil {
		return "toml"
	}

	return "unknown"
}

// iniToMap 将INI结构转换为Map
func iniToMap(f *ini.File) map[string]interface{} {
	result := make(map[string]interface{})
	for _, section := range f.Sections() {
		sectionMap := make(map[string]interface{})
		for _, key := range section.Keys() {
			sectionMap[key.Name()] = key.Value()
		}
		result[section.Name()] = sectionMap
	}
	return result
}

// LoadConfigure 加载配置内容并反序列化（支持多格式）
func LoadConfigure(b []byte, c any, strict bool) error {
	format := DetectConfigFormat(b)
	if format == "unknown" {
		return fmt.Errorf("unsupported config format. Could not detect from content")
	}

	v1.DisallowUnknownFieldsMu.Lock()
	defer v1.DisallowUnknownFieldsMu.Unlock()
	v1.DisallowUnknownFields = strict

	switch format {
	case "toml":
		var tomlObj any
		if err := toml.Unmarshal(b, &tomlObj); err != nil {
			return err
		}
		jsonBytes, err := json.Marshal(&tomlObj)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(jsonBytes))
		if strict {
			decoder.DisallowUnknownFields()
		}
		return decoder.Decode(c)

	case "json":
		decoder := json.NewDecoder(bytes.NewBuffer(b))
		if strict {
			decoder.DisallowUnknownFields()
		}
		return decoder.Decode(c)

	case "ini":
		f, err := ini.Load(b)
		if err != nil {
			return err
		}
		jsonData, err := json.Marshal(iniToMap(f))
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewBuffer(jsonData))
		if strict {
			decoder.DisallowUnknownFields()
		}
		return decoder.Decode(c)
	}

	return fmt.Errorf("unsupported config format: %s", format)
}

func NewProxyConfigurerFromMsg(m *msg.NewProxy, serverCfg *v1.ServerConfig) (v1.ProxyConfigurer, error) {
	m.ProxyType = util.EmptyOr(m.ProxyType, string(v1.ProxyTypeTCP))

	configurer := v1.NewProxyConfigurerByType(v1.ProxyType(m.ProxyType))
	if configurer == nil {
		return nil, fmt.Errorf("unknown proxy type: %s", m.ProxyType)
	}

	configurer.UnmarshalFromMsg(m)
	configurer.Complete("")

	if err := validation.ValidateProxyConfigurerForServer(configurer, serverCfg); err != nil {
		return nil, err
	}
	return configurer, nil
}

func LoadServerConfig(path string, strict bool) (*v1.ServerConfig, bool, error) {
	var (
		svrCfg         *v1.ServerConfig
		isLegacyFormat bool
	)
	// detect legacy ini format
	if DetectLegacyINIFormatFromFile(path) {
		content, err := legacy.GetRenderedConfFromFile(path)
		if err != nil {
			return nil, true, err
		}
		legacyCfg, err := legacy.UnmarshalServerConfFromIni(content)
		if err != nil {
			return nil, true, err
		}
		svrCfg = legacy.Convert_ServerCommonConf_To_v1(&legacyCfg)
		isLegacyFormat = true
	} else {
		svrCfg = &v1.ServerConfig{}
		if err := LoadConfigureFromFile(path, svrCfg, strict); err != nil {
			return nil, false, err
		}
	}
	if svrCfg != nil {
		if err := svrCfg.Complete(); err != nil {
			return nil, isLegacyFormat, err
		}
	}
	return svrCfg, isLegacyFormat, nil
}

func LoadClientConfig(path string, strict bool) (
	*v1.ClientCommonConfig,
	[]v1.ProxyConfigurer,
	[]v1.VisitorConfigurer,
	bool, error,
) {
	var (
		cliCfg         *v1.ClientCommonConfig
		proxyCfgs      = make([]v1.ProxyConfigurer, 0)
		visitorCfgs    = make([]v1.VisitorConfigurer, 0)
		isLegacyFormat bool
	)

	if DetectLegacyINIFormatFromFile(path) {
		legacyCommon, legacyProxyCfgs, legacyVisitorCfgs, err := legacy.ParseClientConfig(path)
		if err != nil {
			return nil, nil, nil, true, err
		}
		cliCfg = legacy.Convert_ClientCommonConf_To_v1(&legacyCommon)
		for _, c := range legacyProxyCfgs {
			proxyCfgs = append(proxyCfgs, legacy.Convert_ProxyConf_To_v1(c))
		}
		for _, c := range legacyVisitorCfgs {
			visitorCfgs = append(visitorCfgs, legacy.Convert_VisitorConf_To_v1(c))
		}
		isLegacyFormat = true
	} else {
		allCfg := v1.ClientConfig{}
		if err := LoadConfigureFromFile(path, &allCfg, strict); err != nil {
			return nil, nil, nil, false, err
		}
		cliCfg = &allCfg.ClientCommonConfig
		for _, c := range allCfg.Proxies {
			proxyCfgs = append(proxyCfgs, c.ProxyConfigurer)
		}
		for _, c := range allCfg.Visitors {
			visitorCfgs = append(visitorCfgs, c.VisitorConfigurer)
		}
	}

	// Load additional config from includes.
	// legacy ini format already handle this in ParseClientConfig.
	if len(cliCfg.IncludeConfigFiles) > 0 && !isLegacyFormat {
		extProxyCfgs, extVisitorCfgs, err := LoadAdditionalClientConfigs(cliCfg.IncludeConfigFiles, isLegacyFormat, strict)
		if err != nil {
			return nil, nil, nil, isLegacyFormat, err
		}
		proxyCfgs = append(proxyCfgs, extProxyCfgs...)
		visitorCfgs = append(visitorCfgs, extVisitorCfgs...)
	}

	// Filter by start
	if len(cliCfg.Start) > 0 {
		startSet := sets.New(cliCfg.Start...)
		proxyCfgs = lo.Filter(proxyCfgs, func(c v1.ProxyConfigurer, _ int) bool {
			return startSet.Has(c.GetBaseConfig().Name)
		})
		visitorCfgs = lo.Filter(visitorCfgs, func(c v1.VisitorConfigurer, _ int) bool {
			return startSet.Has(c.GetBaseConfig().Name)
		})
	}

	if cliCfg != nil {
		if err := cliCfg.Complete(); err != nil {
			return nil, nil, nil, isLegacyFormat, err
		}
	}
	for _, c := range proxyCfgs {
		c.Complete(cliCfg.User)
	}
	for _, c := range visitorCfgs {
		c.Complete(cliCfg)
	}
	return cliCfg, proxyCfgs, visitorCfgs, isLegacyFormat, nil
}

func LoadAdditionalClientConfigs(paths []string, isLegacyFormat bool, strict bool) ([]v1.ProxyConfigurer, []v1.VisitorConfigurer, error) {
	proxyCfgs := make([]v1.ProxyConfigurer, 0)
	visitorCfgs := make([]v1.VisitorConfigurer, 0)
	for _, path := range paths {
		absDir, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return nil, nil, err
		}
		if _, err := os.Stat(absDir); os.IsNotExist(err) {
			return nil, nil, err
		}
		files, err := os.ReadDir(absDir)
		if err != nil {
			return nil, nil, err
		}
		for _, fi := range files {
			if fi.IsDir() {
				continue
			}
			absFile := filepath.Join(absDir, fi.Name())
			if matched, _ := filepath.Match(filepath.Join(absDir, filepath.Base(path)), absFile); matched {
				cfg := v1.ClientConfig{}
				if err := LoadConfigureFromFile(absFile, &cfg, strict); err != nil {
					return nil, nil, fmt.Errorf("load additional config from %s error: %v", absFile, err)
				}
				for _, c := range cfg.Proxies {
					proxyCfgs = append(proxyCfgs, c.ProxyConfigurer)
				}
				for _, c := range cfg.Visitors {
					visitorCfgs = append(visitorCfgs, c.VisitorConfigurer)
				}
			}
		}
	}
	return proxyCfgs, visitorCfgs, nil
}

// 模板函数实现
func parseNumberRange(s string) ([]int, error) {
	return util.ParseNumberRange(s)
}

func parseNumberRangePair(s string) ([][2]int, error) {
	return util.ParseNumberRangePair(s)
}
