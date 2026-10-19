package client

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"html"
	"image"
	// Register the decoders image.DecodeConfig needs for the accepted types.
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"github.com/rahulbalajee/blogctl/internal/rules"
)

// CheckImage applies the site's image rules locally: PNG or JPEG content, 1
// byte to 3 MiB, and at most rules.MaxImageDimension pixels each way. It
// returns the content type to declare. A refused grant still costs a
// mutation attempt, so problems are caught first.
func CheckImage(data []byte) (string, error) {
	if len(data) == 0 || len(data) > rules.MaxImageSize {
		return "", invalid("image must be between 1 byte and %d MiB", rules.MaxImageSize>>20)
	}
	contentType := http.DetectContentType(data)
	if _, ok := rules.ImageExtension(contentType); !ok {
		return "", invalid("image must be PNG or JPEG, not %s", contentType)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", invalid("image could not be decoded: %v", err)
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > rules.MaxImageDimension || cfg.Height > rules.MaxImageDimension {
		return "", invalid("image is %d×%d pixels; at most %d×%d is accepted", cfg.Width, cfg.Height, rules.MaxImageDimension, rules.MaxImageDimension)
	}
	return contentType, nil
}

// SignUpload asks for a storage upload grant for an image of the given type
// and size, attached to the client's draft slug.
func (c *Client) SignUpload(ctx context.Context, slug, contentType string, size int) (UploadGrant, error) {
	if !rules.ValidSlug(slug) {
		return UploadGrant{}, invalidSlug(slug)
	}
	body, err := encodeRequest(struct {
		Slug        string `json:"slug"`
		ContentType string `json:"contentType"`
		Size        int    `json:"size"`
	}{slug, contentType, size})
	if err != nil {
		return UploadGrant{}, err
	}
	resp, err := c.send(ctx, request{
		method:      http.MethodPost,
		path:        "/api/v1/media/sign",
		body:        body,
		contentType: "application/json",
		auth:        authBearer,
		maxBody:     maxSmallResponse,
	})
	if err != nil {
		return UploadGrant{}, err
	}
	if resp.status != http.StatusOK {
		return UploadGrant{}, resp.failure(fmt.Sprintf("post %q", slug))
	}
	var grant UploadGrant
	if err := decodeResponse(resp, &grant); err != nil {
		return UploadGrant{}, err
	}
	if grant.URL == "" || len(grant.Fields) == 0 || !strings.HasPrefix(grant.PreviewURL, "/api/admin/media/"+slug+"/") {
		return UploadGrant{}, &Error{Kind: KindUnknown, RequestID: resp.requestID, Message: "the upload grant was malformed"}
	}
	return grant, nil
}

// Upload sends the image to storage with exactly the grant's form fields.
// The request carries no site credentials or token: storage authorizes it by
// the grant alone.
func (c *Client) Upload(ctx context.Context, grant UploadGrant, filename, contentType string, data []byte) error {
	target, err := url.Parse(grant.URL)
	if err != nil || target.Host == "" || target.User != nil ||
		(target.Scheme != "https" && !(target.Scheme == "http" && c.base.Scheme == "http" && loopbackHost(target.Hostname()))) {
		return &Error{Kind: KindUnknown, Message: "the upload grant URL is not an HTTPS address"}
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	names := make([]string, 0, len(grant.Fields))
	for name := range grant.Fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := writer.WriteField(name, grant.Fields[name]); err != nil {
			return &Error{Kind: KindUnknown, Message: "could not build the upload form", Err: err}
		}
	}
	// Storage reads every field before the file, so the file goes last.
	part, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(filename))},
		"Content-Type":        {contentType},
	})
	if err == nil {
		_, err = part.Write(data)
	}
	if err == nil {
		err = writer.Close()
	}
	if err != nil {
		return &Error{Kind: KindUnknown, Message: "could not build the upload form", Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), &form)
	if err != nil {
		return &Error{Kind: KindUnknown, Message: "could not build the upload request", Err: err}
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("User-Agent", c.userAgent)
	res, err := c.upload.Do(req)
	if err != nil {
		return transportError(ctx, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxSmallResponse))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	// Storage errors are XML; report only their code, never the form.
	var failure struct {
		Code string `xml:"Code"`
	}
	_ = xml.Unmarshal(body, &failure)
	kind := KindInvalid
	if res.StatusCode >= 500 {
		kind = KindUnavailable
	}
	return &Error{Kind: kind, Status: res.StatusCode, Code: failure.Code, Message: "storage refused the upload; the grant may have expired or the image no longer matches it"}
}

// UploadedImage is an image ready to place in an article.
type UploadedImage struct {
	// PreviewURL is the image's source for the article body.
	PreviewURL  string `json:"previewUrl"`
	HTML        string `json:"html"`
	ContentType string `json:"contentType"`
	Size        int    `json:"size"`
}

// UploadImage checks an image locally, requests a grant for slug and uploads
// it. The returned HTML belongs in the article body; nothing is saved until
// the draft is updated with it.
func (c *Client) UploadImage(ctx context.Context, slug, filename string, data []byte, alt string) (UploadedImage, error) {
	if strings.TrimSpace(alt) == "" {
		return UploadedImage{}, invalid("images need descriptive alt text")
	}
	contentType, err := CheckImage(data)
	if err != nil {
		return UploadedImage{}, err
	}
	grant, err := c.SignUpload(ctx, slug, contentType, len(data))
	if err != nil {
		return UploadedImage{}, err
	}
	if err := c.Upload(ctx, grant, filename, contentType, data); err != nil {
		return UploadedImage{}, err
	}
	return UploadedImage{
		PreviewURL:  grant.PreviewURL,
		HTML:        fmt.Sprintf(`<img src="%s" alt="%s">`, html.EscapeString(grant.PreviewURL), html.EscapeString(strings.TrimSpace(alt))),
		ContentType: contentType,
		Size:        len(data),
	}, nil
}
