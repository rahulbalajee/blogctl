package rules

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// The constants must match the site's published contract. testdata holds a
// copy of its OpenAPI document; testdata/SOURCE names the site commit.
func TestRulesMatchThePublishedContract(t *testing.T) {
	raw, err := os.ReadFile("testdata/blog-api.openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Description string `json:"description"`
					MaxItems    int    `json:"maxItems"`
					Items       struct {
						Description string `json:"description"`
					} `json:"items"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	article := spec.Components.Schemas["Article"].Properties
	for field, want := range map[string]string{
		"title":   fmt.Sprintf("%d–%d", MinTitleLength, MaxTitleLength),
		"summary": fmt.Sprintf("%d–%d", MinSummaryLength, MaxSummaryLength),
		"body":    fmt.Sprintf("1–%d bytes", MaxBodyBytes),
	} {
		if !strings.Contains(article[field].Description, want) {
			t.Errorf("article %s: contract says %q, rules say %s", field, article[field].Description, want)
		}
	}
	if !strings.Contains(article["body"].Description, fmt.Sprintf("up to %d distinct", MaxArticleImages)) {
		t.Errorf("article images: contract says %q, rules say %d", article["body"].Description, MaxArticleImages)
	}
	if tags := article["tags"]; tags.MaxItems != MaxTags || !strings.Contains(tags.Items.Description, fmt.Sprintf("1–%d", MaxTagLength)) {
		t.Errorf("tags: contract maxItems %d (%q), rules %d of up to %d bytes", tags.MaxItems, tags.Items.Description, MaxTags, MaxTagLength)
	}

	// Every slug in the contract, as a path parameter or request field, uses
	// the same pattern and length; the media grant states types and size.
	text := string(raw)
	if !strings.Contains(text, `"maxLength": 96`) || strings.Count(text, `"pattern": "`+slugPattern.String()+`"`) < 4 || MaxSlugLength != 96 {
		t.Errorf("slug pattern or length in the contract differs from %s, %d", slugPattern, MaxSlugLength)
	}
	var sign struct {
		Post struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Properties struct {
							ContentType struct {
								Enum []string `json:"enum"`
							} `json:"contentType"`
							Size struct {
								Maximum int `json:"maximum"`
							} `json:"size"`
						} `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"post"`
	}
	media, _ := json.Marshal(spec.Paths["/api/v1/media/sign"])
	if err := json.Unmarshal(media, &sign); err != nil {
		t.Fatal(err)
	}
	props := sign.Post.RequestBody.Content["application/json"].Schema.Properties
	if props.Size.Maximum != MaxImageSize {
		t.Errorf("image size: contract %d, rules %d", props.Size.Maximum, MaxImageSize)
	}
	var accepted []string
	for _, typ := range []string{"image/png", "image/jpeg", "image/gif", "image/webp"} {
		if _, ok := ImageExtension(typ); ok {
			accepted = append(accepted, typ)
		}
	}
	slices.Sort(accepted)
	contract := slices.Clone(props.ContentType.Enum)
	slices.Sort(contract)
	if !slices.Equal(accepted, contract) {
		t.Errorf("image types: contract %v, rules %v", contract, accepted)
	}
}

func TestValidSlug(t *testing.T) {
	for slug, want := range map[string]bool{
		"notes":                 true,
		"go-1-26-notes":         true,
		"Notes":                 false,
		"double--hyphen":        false,
		"-leading":              false,
		"":                      false,
		strings.Repeat("a", 96): true,
		strings.Repeat("a", 97): false,
		"with space":            false,
	} {
		if ValidSlug(slug) != want {
			t.Errorf("ValidSlug(%q) = %v", slug, !want)
		}
	}
}
