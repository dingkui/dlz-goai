// Package jsonutil 提供 JSON 文件读写的小工具，供各包的持久化实现共用。
package jsonutil

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrNotFound 文件不存在。调用方据此区分"还没有配置"与"配置损坏"。
var ErrNotFound = errors.New("配置文件不存在")

// LoadFile 读取 JSON 文件到 v。文件不存在时返回 ErrNotFound。
func LoadFile(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// SaveFile 原子写入 JSON 文件：先写同目录临时文件再重命名，
// 避免写入过程中进程被杀导致配置损坏。
func SaveFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// repairSuffixes 截断的 JSON 通常缺闭合符：多半个字符串、
// 少一个 } 或 ]}。逐个尝试补全。
var repairSuffixes = []string{`}`, `"}`, `]}`, `"}]}`, `"]}`}

// Repair 抢救被截断的 JSON：先直接解析，再尝试补闭合符并逐字符回退。
// O(n²) 但修复是罕见路径，正常读写不走这里。
func Repair(b []byte, v any) error {
	try := func(data []byte) bool {
		return json.Unmarshal(data, v) == nil
	}
	if try(b) {
		return nil
	}
	for _, suffix := range repairSuffixes {
		if try(append(append([]byte(nil), b...), suffix...)) {
			return nil
		}
	}
	for len(b) > 0 {
		b = b[:len(b)-1]
		for _, suffix := range repairSuffixes {
			if try(append(append([]byte(nil), b...), suffix...)) {
				return nil
			}
		}
	}
	return fmt.Errorf("无法修复 JSON")
}
