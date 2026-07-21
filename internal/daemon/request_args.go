package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"gpuardian/internal/protocol"
)

const (
	maxRunCommandArgs  = 1024
	maxRunCommandBytes = 256 << 10
	maxRunEnvEntries   = 4096
	maxRunEnvBytes     = 512 << 10
)

func decodeRegisterArgs(raw json.RawMessage) (protocol.RegisterArgs, error) {
	var args protocol.RegisterArgs
	err := decodeRPCArgsObject(raw, func(decoder *json.Decoder, field string) error {
		switch field {
		case "root_key":
			return decoder.Decode(&args.RootKey)
		case "mode":
			return decoder.Decode(&args.Mode)
		case "name":
			return decoder.Decode(&args.Name)
		case "purpose":
			return decoder.Decode(&args.Purpose)
		case "external_session_id":
			return decoder.Decode(&args.ExternalSessionID)
		case "user_key_id":
			return decoder.Decode(&args.UserKeyID)
		case "gpus":
			return decodeBoundedIntArray(decoder, &args.GPUs, maxGPUsPerRequest, "gpus")
		case "ttl":
			return decoder.Decode(&args.TTL)
		case "starts_at":
			return decoder.Decode(&args.StartsAt)
		case "expires_at":
			return decoder.Decode(&args.ExpiresAt)
		default:
			return fmt.Errorf("unknown register argument %q", field)
		}
	})
	return args, err
}

func decodeRunArgs(raw json.RawMessage) (protocol.RunArgs, error) {
	var args protocol.RunArgs
	err := decodeRPCArgsObject(raw, func(decoder *json.Decoder, field string) error {
		switch field {
		case "command":
			return decodeBoundedStringArray(decoder, &args.Command, maxRunCommandArgs, maxRunCommandBytes, "command")
		case "workdir":
			return decoder.Decode(&args.Workdir)
		case "env":
			return decodeBoundedStringArray(decoder, &args.Env, maxRunEnvEntries, maxRunEnvBytes, "env")
		default:
			return fmt.Errorf("unknown run argument %q", field)
		}
	})
	if err != nil {
		return protocol.RunArgs{}, err
	}
	if err := validateRunArgs(args); err != nil {
		return protocol.RunArgs{}, err
	}
	return args, nil
}

func decodeRPCArgsObject(raw json.RawMessage, decodeField func(*json.Decoder, string) error) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("RPC arguments must be a JSON object")
	}

	seen := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := token.(string)
		if !ok {
			return errors.New("RPC argument name must be a string")
		}
		if _, duplicate := seen[field]; duplicate {
			return fmt.Errorf("duplicate RPC argument %q", field)
		}
		seen[field] = struct{}{}
		if err := decodeField(decoder, field); err != nil {
			return err
		}
	}
	if token, err = decoder.Token(); err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return errors.New("RPC arguments must end with a JSON object delimiter")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("RPC arguments contain trailing data")
		}
		return err
	}
	return nil
}

func decodeBoundedIntArray(decoder *json.Decoder, out *[]int, maxEntries int, label string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		*out = nil
		return nil
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '[' {
		return fmt.Errorf("%s must be an array", label)
	}
	values := make([]int, 0, min(maxEntries, 16))
	for decoder.More() {
		if len(values) >= maxEntries {
			return fmt.Errorf("%s has more than %d entries", label, maxEntries)
		}
		var value int
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode %s entry: %w", label, err)
		}
		values = append(values, value)
	}
	if token, err = decoder.Token(); err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != ']' {
		return fmt.Errorf("%s must end with an array delimiter", label)
	}
	*out = values
	return nil
}

func decodeBoundedStringArray(decoder *json.Decoder, out *[]string, maxEntries, maxBytes int, label string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		*out = nil
		return nil
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '[' {
		return fmt.Errorf("%s must be an array", label)
	}
	values := make([]string, 0, min(maxEntries, 16))
	totalBytes := 0
	for decoder.More() {
		if len(values) >= maxEntries {
			return fmt.Errorf("%s has more than %d entries", label, maxEntries)
		}
		var value string
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("decode %s entry: %w", label, err)
		}
		if len(value) > maxBytes-totalBytes {
			return fmt.Errorf("%s exceeds %d bytes", label, maxBytes)
		}
		totalBytes += len(value)
		values = append(values, value)
	}
	if token, err = decoder.Token(); err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != ']' {
		return fmt.Errorf("%s must end with an array delimiter", label)
	}
	*out = values
	return nil
}

func validateRunArgs(args protocol.RunArgs) error {
	if len(args.Command) == 0 {
		return errors.New("command is required")
	}
	if err := validateStringArray(args.Command, maxRunCommandArgs, maxRunCommandBytes, "command"); err != nil {
		return err
	}
	if err := validateStringArray(args.Env, maxRunEnvEntries, maxRunEnvBytes, "env"); err != nil {
		return err
	}
	return validateRequestValue("workdir", args.Workdir)
}

func validateStringArray(values []string, maxEntries, maxBytes int, label string) error {
	if len(values) > maxEntries {
		return fmt.Errorf("%s has more than %d entries", label, maxEntries)
	}
	totalBytes := 0
	for _, value := range values {
		if len(value) > maxBytes-totalBytes {
			return fmt.Errorf("%s exceeds %d bytes", label, maxBytes)
		}
		totalBytes += len(value)
	}
	return nil
}
