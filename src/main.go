package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os/exec"
	"strings"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/namespaces"
)

const containerdSocket = "/run/containerd/containerd.sock"

type ApplyLatencyRequest struct {
	ContainerID string `json:"container_id"`
	Delay       string `json:"delay"` // ex: "200ms"
	TargetIP    string `json:"target_ip"`
}

func hasRootHTB(pid string) bool {
	cmd := exec.Command("nsenter", "-t", pid, "-n", "tc", "qdisc", "show", "dev", "eth0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}

	return bytes.Contains(output, []byte("qdisc htb 1: root"))
}

func generateClassIDMinor(ip string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(ip))

	minor := h.Sum32() & 0xFFFF

	if minor == 0 {
		minor = 1
	}

	return minor
}

func getPIDFromContainerID(containerID string) (string, error) {
	cleanID := strings.TrimPrefix(containerID, "containerd://")

	client, err := containerd.New(containerdSocket)
	if err != nil {
		return "", fmt.Errorf("error connecting to containerd: %v", err)
	}
	defer client.Close()

	ctx := namespaces.WithNamespace(context.Background(), "k8s.io")

	container, err := client.LoadContainer(ctx, cleanID)
	if err != nil {
		return "", fmt.Errorf("container not found: %v", err)
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("error retrieving the task from container: %v", err)
	}

	pid := fmt.Sprintf("%d", task.Pid())
	return pid, nil
}

func applyLatencyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	var req ApplyLatencyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pidStr, err := getPIDFromContainerID(req.ContainerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	log.Printf("Applying latency in PID: %s)", pidStr)

	minorNum := generateClassIDMinor(req.TargetIP)
	minorID := fmt.Sprintf("%x", minorNum)
	classID := "1:" + minorID
	handleID := minorID + ":"
	prioStr := fmt.Sprintf("%d", minorNum)

	var commands [][]string
	if !hasRootHTB(pidStr) {
		commands = append(commands, []string{"nsenter", "-t", pidStr, "-n", "tc", "qdisc", "add", "dev", "eth0", "root", "handle", "1:", "htb"})
	} else {
		log.Printf("Qdisc HTB already exists in PID %s. Skipping creation.", pidStr)
	}
	commands = append(commands, []string{"nsenter", "-t", pidStr, "-n", "tc", "filter", "del", "dev", "eth0", "parent", "1:0", "prio", prioStr})
	commands = append(commands, []string{"nsenter", "-t", pidStr, "-n", "tc", "class", "replace", "dev", "eth0", "parent", "1:", "classid", classID, "htb", "rate", "1000mbit"})
	commands = append(commands, []string{"nsenter", "-t", pidStr, "-n", "tc", "qdisc", "replace", "dev", "eth0", "parent", classID, "handle", handleID, "netem", "delay", req.Delay})
	commands = append(commands, []string{"nsenter", "-t", pidStr, "-n", "tc", "filter", "add", "dev", "eth0", "protocol", "ip", "parent", "1:0", "prio", prioStr, "u32", "match", "ip", "dst", req.TargetIP + "/32", "flowid", classID})
	for _, args := range commands {
		cmd := exec.Command(args[0], args[1:]...)
		output, err := cmd.CombinedOutput()
		if err != nil && args[6] != "del" {
			log.Printf("Error executing '%v': %s - %v", args, string(output), err)
			http.Error(w, fmt.Sprintf("Error applying tc: %s", string(output)), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Latency of %s successfully applied in PID %s for target %s\n", req.Delay, pidStr, req.TargetIP)
}

type DeleteLatencyRequest struct {
	ContainerID string `json:"container_id"`
	TargetIP    string `json:"target_ip"`
}

func deleteLatencyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	var req DeleteLatencyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pidStr, err := getPIDFromContainerID(req.ContainerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	minorNum := generateClassIDMinor(req.TargetIP)
	minorID := fmt.Sprintf("%x", minorNum)
	classID := "1:" + minorID
	prioStr := fmt.Sprintf("%d", minorNum)

	cmdFilter := exec.Command("nsenter", "-t", pidStr, "-n", "tc", "filter", "del", "dev", "eth0", "parent", "1:0", "prio", prioStr)
	cmdFilter.CombinedOutput()

	cmd := exec.Command("nsenter", "-t", pidStr, "-n", "tc", "class", "del", "dev", "eth0", "classid", classID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "No such file or directory") || strings.Contains(string(output), "We have an error talking to the kernel") {
			log.Printf("Class %s didnt exist in PID %s. Ignoring.", classID, pidStr)
		} else {
			log.Printf("Error deleting class %s: %s - %v", classID, string(output), err)
			http.Error(w, fmt.Sprintf("Error deleting rule tc: %s", string(output)), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "Latency to the target %s successfully removed in PID %s\n", req.TargetIP, pidStr)
}

type ClearLatencyRequest struct {
	ContainerID string `json:"container_id"`
}

func clearLatencyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not supported", http.StatusMethodNotAllowed)
		return
	}

	var req ClearLatencyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	pidStr, err := getPIDFromContainerID(req.ContainerID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cmd := exec.Command("nsenter", "-t", pidStr, "-n", "tc", "qdisc", "del", "dev", "eth0", "root")
	output, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "No such file or directory") || strings.Contains(string(output), "Cannot delete qdisc") {
			log.Printf("No network rule found in PID %s. It was already empty.", pidStr)
		} else {
			log.Printf("Error cleaning qdisc root: %s - %v", string(output), err)
			http.Error(w, fmt.Sprintf("Error cleaning network: %s", string(output)), http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "All latency rules have been removed from the PID container %s\n", pidStr)
}

func main() {
	http.HandleFunc("/api/apply-latency", applyLatencyHandler)
	http.HandleFunc("/api/delete-latency", deleteLatencyHandler)
	http.HandleFunc("/api/clear-latency", clearLatencyHandler)

	log.Println("Server running on port 8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Error starting the server HTTP: %v", err)
	}
}
