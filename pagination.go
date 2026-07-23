package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

func (c *githubClient) paginate(path string, params url.Values, itemsKey string, handle func(json.RawMessage) error) error {
	params = cloneValues(params)
	params.Set("per_page", "100")
	nextURL := c.baseURL + path + "?" + params.Encode()
	page := 1
	for nextURL != "" {
		currentURL := nextURL
		body, link, err := c.request(nextURL)
		if err != nil {
			return err
		}
		items, err := extractItems(body, itemsKey)
		if err != nil {
			return err
		}
		c.debugf("page %d %s returned %d items", page, requestURLForLog(currentURL), len(items))
		for _, item := range items {
			if err := handle(item); err != nil {
				return err
			}
		}
		nextURL = nextLink(link)
		if nextURL != "" {
			c.debugf("next page: %s", requestURLForLog(nextURL))
		}
		page++
	}
	return nil
}

func cloneValues(values url.Values) url.Values {
	out := url.Values{}
	for key, vals := range values {
		for _, value := range vals {
			out.Add(key, value)
		}
	}
	return out
}

func extractItems(body []byte, itemsKey string) ([]json.RawMessage, error) {
	if itemsKey == "" {
		var items []json.RawMessage
		return items, json.Unmarshal(body, &items)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	raw, ok := obj[itemsKey]
	if !ok || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var items []json.RawMessage
	return items, json.Unmarshal(raw, &items)
}

func nextLink(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		rawURL := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		for _, seg := range segs[1:] {
			if strings.TrimSpace(seg) == `rel="next"` {
				return rawURL
			}
		}
	}
	return ""
}
