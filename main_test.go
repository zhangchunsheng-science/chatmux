package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSkills(t *testing.T) {
	dir := t.TempDir()
	// flat skill
	os.WriteFile(filepath.Join(dir, "flat.md"), []byte("直接正文"), 0o644)
	// flat skill with frontmatter
	os.WriteFile(filepath.Join(dir, "meta.md"), []byte("---\nname: x\ndescription: '一段描述'\n---\n正文内容"), 0o644)
	// directory skill (Claude style)
	os.Mkdir(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "SKILL.md"), []byte("---\ndescription: 目录技能\n---\n包技能正文"), 0o644)
	// non-skill noise
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(dir, "empty"), 0o755)

	bodies, descs := loadSkills(dir)
	if len(bodies) != 3 {
		t.Fatalf("want 3 skills, got %d: %v", len(bodies), bodies)
	}
	if bodies["flat"] != "直接正文" {
		t.Errorf("flat: %q", bodies["flat"])
	}
	if bodies["meta"] != "正文内容" {
		t.Errorf("meta body should be frontmatter-stripped: %q", bodies["meta"])
	}
	if descs["meta"] != "一段描述" || descs["pkg"] != "目录技能" {
		t.Errorf("descs: %+v", descs)
	}
	if _, ok := bodies["empty"]; ok {
		t.Error("dir without SKILL.md should not load")
	}
	if _, ok := bodies["notes"]; ok {
		t.Error("non-md should not load")
	}
}

func TestStripFrontmatterNoBlock(t *testing.T) {
	body, desc := stripFrontmatter("just text")
	if body != "just text" || desc != "" {
		t.Fatalf("%q %q", body, desc)
	}
	body, desc = stripFrontmatter("---\nno closing fence")
	if body != "---\nno closing fence" || desc != "" {
		t.Fatalf("unclosed frontmatter should pass through: %q", body)
	}
}
