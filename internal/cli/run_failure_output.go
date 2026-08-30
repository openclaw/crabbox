package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// failureBundleOutput owns the verified directory and its file until Close.
// Pathnames are only for reporting; mutations use the retained directory.
type failureBundleOutput struct {
	directory failureBundleDirectory
	file      *os.File
	name      string
	published bool
	closed    bool
}

func (o *failureBundleOutput) createTemp(name string) error {
	if o.closed || o.file != nil {
		return fmt.Errorf("failure bundle output is not available for creation")
	}
	if name == "." || !filepath.IsLocal(name) || filepath.Base(name) != name {
		return fmt.Errorf("invalid failure bundle name %q", name)
	}
	for attempt := 0; attempt < 32; attempt++ {
		token, err := randomHex(12)
		if err != nil {
			return err
		}
		tempName := "." + name + ".crabbox-" + token
		file, err := o.createFile(tempName)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		o.file, o.name = file, tempName
		return o.secureFile()
	}
	return fmt.Errorf("allocate failure bundle temporary file")
}

func (o *failureBundleOutput) publish(name string) error {
	if o.closed || o.file == nil || o.published {
		return fmt.Errorf("failure bundle output is not available for publication")
	}
	if name == "." || !filepath.IsLocal(name) || filepath.Base(name) != name {
		return fmt.Errorf("invalid failure bundle name %q", name)
	}
	if err := o.validateDestination(name); err != nil {
		return err
	}
	if err := o.rename(name); err != nil {
		return privateRunOutputWriteError{err}
	}
	o.name, o.published = name, true
	return nil
}

func (o *failureBundleOutput) Write(p []byte) (int, error) {
	if o.closed || o.file == nil {
		return 0, os.ErrClosed
	}
	return o.file.Write(p)
}

func (o *failureBundleOutput) Close() error {
	if o.closed {
		return nil
	}
	o.closed = true
	var err error
	if o.file != nil {
		if !o.published {
			err = o.remove()
		}
		err = errors.Join(err, o.file.Close())
	}
	return errors.Join(err, o.closeDirectory())
}
