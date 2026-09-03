package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestOneDriveDeltaPathAndLinkHostPinning(t *testing.T) {
	cfg := oneDriveConfig{DriveID: "b!drive-id"}
	path, err := oneDriveInitialDeltaPath(t.Context(), cfg, "token")
	if err != nil {
		t.Fatalf("oneDriveInitialDeltaPath: %v", err)
	}
	if path != "/drives/b%21drive-id/root/delta" {
		t.Fatalf("unexpected delta path %q", path)
	}

	oldBase := graphBaseURL
	graphBaseURL = "https://graph.example.test/v1.0"
	t.Cleanup(func() { graphBaseURL = oldBase })
	got, err := oneDriveGraphPathFromLink("https://graph.example.test/v1.0/drives/x/root/delta?$skiptoken=abc")
	if err != nil || got != "/v1.0/drives/x/root/delta?$skiptoken=abc" {
		t.Fatalf("accepted Graph cursor = %q, %v", got, err)
	}
	if _, err := oneDriveGraphPathFromLink("https://attacker.example/delta?token=abc"); err == nil {
		t.Fatal("foreign delta link was accepted")
	}
}

func TestGitHubImporterRequiresHTTPSAndRendersPullRequest(t *testing.T) {
	if _, err := githubBaseURL(githubConfig{BaseURL: "http://github.example.test"}); err == nil {
		t.Fatal("non-HTTPS GitHub API URL was accepted")
	}
	item := githubIssue{Number: 42, Title: "Dokumentation", State: "open", Body: "Bitte ergänzen."}
	item.User.Login = "ada"
	item.PullRequest = &struct {
		URL string `json:"url"`
	}{URL: "https://api.github.com/repos/o/r/pulls/42"}
	text := githubIssueText(item, true)
	for _, want := range []string{"Pull Request #42", "Autor: ada", "Bitte ergänzen."} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered GitHub text lacks %q: %q", want, text)
		}
	}
}

func TestSAPS4ODataParsingAndPinnedCursor(t *testing.T) {
	v4, err := parseSAPS4Page([]byte(`{"value":[{"BusinessPartner":"100","Name":"Ada"}],"@odata.nextLink":"https://s4.example.test/next"}`))
	if err != nil || len(v4.Records) != 1 || v4.NextLink == "" {
		t.Fatalf("parse V4 = %#v, %v", v4, err)
	}
	v2, err := parseSAPS4Page([]byte(`{"d":{"results":[{"BusinessPartner":"200"}],"__next":"https://s4.example.test/next"}}`))
	if err != nil || len(v2.Records) != 1 || v2.NextLink == "" {
		t.Fatalf("parse V2 = %#v, %v", v2, err)
	}

	cfg := sapS4Config{BaseURL: "https://s4.example.test", EntityPath: "/sap/opu/odata/sap/API_BP/A_BusinessPartner", IDField: "BusinessPartner", ContentFields: []string{"Name"}}
	initial, err := sapS4InitialURL(cfg, 10)
	if err != nil {
		t.Fatalf("sapS4InitialURL: %v", err)
	}
	u, err := url.Parse(initial)
	if err != nil || u.Query().Get("$select") != "BusinessPartner,Name" || u.Query().Get("$top") != "10" {
		t.Fatalf("unexpected constrained OData URL %q (%v)", initial, err)
	}
	if _, err := sapS4CursorURL(cfg, "https://attacker.example/next"); err == nil {
		t.Fatal("foreign SAP continuation link was accepted")
	}
	text := sapS4RecordText(cfg, map[string]any{"BusinessPartner": "100", "Name": "Ada GmbH", "Secret": "must not be present"})
	if !strings.Contains(text, "Name: Ada GmbH") || strings.Contains(text, "Secret") {
		t.Fatalf("SAP document did not honor field allow-list: %q", text)
	}
}
