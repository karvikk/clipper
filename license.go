package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

const (
	keygenAccount = "1f489a9e-637f-48d4-8007-3cb882777a74"
	keygenBaseURL = "https://api.keygen.sh/v1/accounts/" + keygenAccount
	keygenSalt    = "clipper-v1"
)

type License struct {
	ID        string
	Remaining *int
}

type licenseResponse struct {
	Data struct {
		ID         string `json:"id"`
		Attributes struct {
			Uses    int  `json:"uses"`
			MaxUses *int `json:"maxUses"`
		} `json:"attributes"`
	} `json:"data"`
	Meta struct {
		Valid  bool   `json:"valid"`
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"meta"`
}

func validateLicense(ctx context.Context, key string) (License, error) {
	if strings.TrimSpace(key) == "" {
		return License{}, errors.New("введите ключ доступа")
	}
	fingerprint, err := machineFingerprint()
	if err != nil {
		return License{}, fmt.Errorf("не удалось проверить компьютер: %w", err)
	}

	response, err := requestLicense(ctx, http.MethodPost, "/licenses/actions/validate-key", "", map[string]any{
		"meta": map[string]any{"key": key, "scope": map[string]string{"fingerprint": fingerprint}},
	})
	if err != nil {
		return License{}, err
	}
	if response.Meta.Code == "NO_MACHINE" || response.Meta.Code == "NO_MACHINES" {
		if response.Data.ID == "" {
			return License{}, errors.New("сервер лицензий не вернул идентификатор ключа")
		}
		body := map[string]any{
			"data": map[string]any{
				"type": "machines",
				"attributes": map[string]string{
					"fingerprint": fingerprint,
					"platform":    "Windows",
					"name":        computerName(),
				},
				"relationships": map[string]any{
					"license": map[string]any{"data": map[string]string{"type": "licenses", "id": response.Data.ID}},
				},
			},
		}
		if err := createMachine(ctx, key, body); err != nil {
			return License{}, err
		}
		response, err = requestLicense(ctx, http.MethodPost, "/licenses/actions/validate-key", "", map[string]any{
			"meta": map[string]any{"key": key, "scope": map[string]string{"fingerprint": fingerprint}},
		})
		if err != nil {
			return License{}, err
		}
	}
	if response.Meta.Code == "FINGERPRINT_SCOPE_MISMATCH" {
		return License{}, errors.New("этот ключ уже привязан к другому компьютеру")
	}
	if !response.Meta.Valid {
		detail := response.Meta.Detail
		if detail == "" {
			detail = response.Meta.Code
		}
		if detail == "" {
			detail = "ключ недействителен"
		}
		return License{}, fmt.Errorf("ключ не принят: %s", detail)
	}
	if response.Data.ID == "" {
		return License{}, errors.New("сервер лицензий не вернул данные ключа")
	}
	var remaining *int
	if response.Data.Attributes.MaxUses != nil {
		value := *response.Data.Attributes.MaxUses - response.Data.Attributes.Uses
		if value < 0 {
			value = 0
		}
		remaining = &value
	}
	return License{ID: response.Data.ID, Remaining: remaining}, nil
}

func incrementLicenseUsage(ctx context.Context, key, licenseID string) error {
	if licenseID == "" {
		return errors.New("у ключа отсутствует идентификатор лицензии")
	}
	endpoint := "/licenses/" + licenseID + "/actions/increment-usage"
	response, status, err := requestJSON(ctx, http.MethodPost, endpoint, key, map[string]any{
		"meta": map[string]int{"increment": 1},
	})
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("сервер лицензий отказал списать клип (HTTP %d): %s", status, responseDetail(response))
	}
	return nil
}

func createMachine(ctx context.Context, key string, body any) error {
	_, status, err := requestJSON(ctx, http.MethodPost, "/machines", key, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return fmt.Errorf("не удалось привязать ключ к этому компьютеру (HTTP %d)", status)
	}
	return nil
}

func requestLicense(ctx context.Context, method, path, key string, body any) (licenseResponse, error) {
	data, status, err := requestJSON(ctx, method, path, key, body)
	if err != nil {
		return licenseResponse{}, err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return licenseResponse{}, fmt.Errorf("сервер лицензий ответил HTTP %d: %s", status, responseDetail(data))
	}
	var response licenseResponse
	if err := json.Unmarshal(data, &response); err != nil {
		return licenseResponse{}, fmt.Errorf("сервер лицензий вернул некорректный ответ: %w", err)
	}
	return response, nil
}

func requestJSON(ctx context.Context, method, path, key string, body any) ([]byte, int, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, 0, fmt.Errorf("не удалось подготовить запрос лицензии: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, method, keygenBaseURL+path, bytes.NewReader(data))
	if err != nil {
		return nil, 0, fmt.Errorf("не удалось подготовить запрос лицензии: %w", err)
	}
	request.Header.Set("Content-Type", "application/vnd.api+json")
	request.Header.Set("Accept", "application/vnd.api+json")
	request.Header.Set("Keygen-Version", "1.8")
	if key != "" {
		request.Header.Set("Authorization", "License "+key)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("нет связи с сервером лицензий: %w", err)
	}
	defer response.Body.Close()
	responseData, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("не удалось прочитать ответ сервера лицензий: %w", err)
	}
	return responseData, response.StatusCode, nil
}

func responseDetail(data []byte) string {
	var response struct {
		Errors []struct {
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"errors"`
		Meta struct {
			Detail string `json:"detail"`
		} `json:"meta"`
	}
	if json.Unmarshal(data, &response) == nil {
		if response.Meta.Detail != "" {
			return response.Meta.Detail
		}
		if len(response.Errors) > 0 {
			if response.Errors[0].Detail != "" {
				return response.Errors[0].Detail
			}
			return response.Errors[0].Title
		}
	}
	return "попробуйте ещё раз позже"
}

func machineFingerprint() (string, error) {
	machineID := ""
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE)
	if err == nil {
		machineID, _, err = key.GetStringValue("MachineGuid")
		key.Close()
	}
	if err != nil || machineID == "" {
		machineID = computerMachineFallback()
	}
	if machineID == "" {
		return "", errors.New("не удалось получить идентификатор устройства")
	}
	sum := sha256.Sum256([]byte(keygenSalt + machineID))
	return hex.EncodeToString(sum[:]), nil
}

func computerMachineFallback() string {
	if interfaces, err := net.Interfaces(); err == nil {
		for _, item := range interfaces {
			if len(item.HardwareAddr) > 0 {
				return item.HardwareAddr.String()
			}
		}
	}
	output, _ := exec.Command("hostname").Output()
	return strings.TrimSpace(string(output))
}

func computerName() string {
	name, err := os.Hostname()
	if err == nil && name != "" {
		return name
	}
	return filepath.Base(os.Getenv("COMPUTERNAME"))
}
