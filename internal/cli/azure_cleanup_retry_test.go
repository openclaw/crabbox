package cli

import (
	"context"
	"errors"
	"testing"
)

func TestRetryAzureCleanupResourceReadsPreservesValidatedIdentities(t *testing.T) {
	t.Parallel()
	original := azureVMDeleteResources{nic: "lease-nic", nicID: "nic-guid"}
	attempts := 0
	got, err := retryAzureCleanupResourceReadsWithWait(context.Background(), original, func(resources azureVMDeleteResources) (azureVMDeleteResources, error) {
		attempts++
		if resources != original {
			t.Fatalf("retry resources=%+v, want original %+v", resources, original)
		}
		if attempts == 1 {
			resources.nic = ""
			return resources, &azureCleanupResourceReadError{err: errors.New("transient read")}
		}
		resources.nic = ""
		return resources, nil
	}, func(context.Context, []error) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || got.nic != "" {
		t.Fatalf("attempts=%d resources=%+v, want two attempts and absent NIC", attempts, got)
	}
}

func TestRetryAzureCleanupResourceReadsDoesNotRetryValidationFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("identity mismatch")
	attempts := 0
	_, err := retryAzureCleanupResourceReadsWithWait(context.Background(), azureVMDeleteResources{}, func(resources azureVMDeleteResources) (azureVMDeleteResources, error) {
		attempts++
		return resources, &azureCleanupSkipError{err: want}
	}, func(context.Context, []error) error {
		t.Fatal("validation failures must not wait for retry")
		return nil
	})
	if !errors.Is(err, want) || attempts != 1 {
		t.Fatalf("error=%v attempts=%d, want validation error without retry", err, attempts)
	}
}

func TestRetryAzureCleanupResourceReadsIsBounded(t *testing.T) {
	t.Parallel()
	want := errors.New("persistent read failure")
	attempts := 0
	_, err := retryAzureCleanupResourceReadsWithWait(context.Background(), azureVMDeleteResources{}, func(resources azureVMDeleteResources) (azureVMDeleteResources, error) {
		attempts++
		return resources, &azureCleanupResourceReadError{err: want}
	}, func(context.Context, []error) error { return nil })
	if !errors.Is(err, want) || attempts != azureDeleteRetryAttempts {
		t.Fatalf("error=%v attempts=%d, want %d bounded attempts", err, attempts, azureDeleteRetryAttempts)
	}
}
