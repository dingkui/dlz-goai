package main

import (
	"bufio"
	"context"
	"encoding/json"
	dlzgoai "github.com/dingkui/dlz-goai"
	"github.com/dingkui/dlz-goai/agent"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPReconnectCancelResumeApprove(t *testing.T) {
	client, _ := dlzgoai.NewClient(dlzgoai.Options{})
	defer client.Close()
	server := httptest.NewServer(newHandler(client))
	defer server.Close()
	httpClient := &http.Client{Timeout: 5 * time.Second}
	request := func(method, path, body string, status int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, _ := io.ReadAll(res.Body)
		if res.StatusCode != status {
			t.Fatalf("%s %s: %d %s", method, path, res.StatusCode, data)
		}
		return data
	}
	var started struct {
		ID string `json:"run_id"`
	}
	if err := json.Unmarshal(request("POST", "/runs", `{"input":"test"}`, 202), &started); err != nil {
		t.Fatal(err)
	}
	path := "/runs/" + started.ID
	request("GET", path, "", 200) // registration must already be visible
	request("POST", path+"/resume", "", 409)
	readUntilApproval := func(after string) int64 {
		t.Helper()
		req, _ := http.NewRequest("GET", server.URL+path+"/events", nil)
		req.Header.Set("Last-Event-ID", after)
		res, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		scanner := bufio.NewScanner(res.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var e agent.Event
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
				t.Fatal(err)
			}
			if e.Type == agent.EventApprovalRequired && e.Approved == nil {
				return e.Seq
			}
		}
		t.Fatalf("approval event missing: %v", scanner.Err())
		return 0
	}
	seq := readUntilApproval("") // disconnecting does not cancel the run
	request("GET", path, "", 200)
	request("POST", path+"/cancel", "", 202)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Wait(ctx, started.ID); err == nil {
		t.Fatal("cancel had no effect")
	}
	request("POST", path+"/resume", "", 202)
	req, _ := http.NewRequest("GET", server.URL+path+"/events", nil)
	req.Header.Set("Last-Event-ID", strconv.FormatInt(seq, 10))
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	scanner := bufio.NewScanner(res.Body)
	last := seq
	sawDone, approved := false, false
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var e agent.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
			t.Fatal(err)
		}
		if e.Seq <= last {
			t.Fatalf("duplicate/out-of-order event: %d <= %d", e.Seq, last)
		}
		last = e.Seq
		if e.Type == agent.EventApprovalRequired && e.Approved == nil {
			request("POST", path+"/approvals/"+e.CallID, `{"approved":true}`, 200)
			approved = true
		}
		if e.Type == agent.EventRunDone {
			sawDone = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !approved || !sawDone {
		t.Fatalf("approved=%v done=%v", approved, sawDone)
	}
	result, err := client.Wait(ctx, started.ID)
	if err != nil || !strings.Contains(result.Content, "approved demo") {
		t.Fatal(result, err)
	}
}
