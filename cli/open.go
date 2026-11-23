package cli

import "os/exec"

// startProcess runs name with arg and doesn't wait: opening a browser.
func startProcess(name, arg string) {
	if cmd := exec.Command(name, arg); cmd.Start() == nil {
		go cmd.Wait()
	}
}
