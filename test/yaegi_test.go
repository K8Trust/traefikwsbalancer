// test/yaegi_test.go
package test

import (
    "os"
    "path/filepath"
    "testing"

    "github.com/traefik/yaegi/interp"
    "github.com/traefik/yaegi/stdlib"
)

func TestYaegiLoad(t *testing.T) {
    // Get the absolute path to the project root
    wd, err := os.Getwd()
    if err != nil {
        t.Fatalf("Failed to get working directory: %v", err)
    }
    rootDir := filepath.Dir(wd)

    // Create temporary GOPATH
    tmpGoPath, err := os.MkdirTemp("", "yaegi-test")
    if err != nil {
        t.Fatalf("Failed to create temp directory: %v", err)
    }
    defer os.RemoveAll(tmpGoPath)

    // Create package directory structure
    pkgPath := filepath.Join(tmpGoPath, "src", "github.com", "K8Trust", "traefikwsbalancer")
    if err := os.MkdirAll(pkgPath, 0755); err != nil {
        t.Fatalf("Failed to create package directory: %v", err)
    }

    // Create ws subdirectory
    wsPath := filepath.Join(pkgPath, "ws")
    if err := os.MkdirAll(wsPath, 0755); err != nil {
        t.Fatalf("Failed to create ws directory: %v", err)
    }

    // Copy necessary files
    filesToCopy := map[string]string{
        "traefikwsbalancer.go": pkgPath,
        "metrics_types.go":     pkgPath,
        "metrics_yaegi.go":     pkgPath,
        "symbols_yaegi.go":     pkgPath,
        "ws/ws.go":            wsPath,
    }

    for src, dst := range filesToCopy {
        srcPath := filepath.Join(rootDir, src)
        content, err := os.ReadFile(srcPath)
        if err != nil {
            t.Fatalf("Failed to read file %s: %v", srcPath, err)
        }
        dstPath := filepath.Join(dst, filepath.Base(src))
        if err := os.WriteFile(dstPath, content, 0644); err != nil {
            t.Fatalf("Failed to write file %s: %v", dstPath, err)
        }
    }

    // Create interpreter
    i := interp.New(interp.Options{
        GoPath: tmpGoPath,
    })

    // Use standard library
    err = i.Use(stdlib.Symbols)
    if err != nil {
        t.Fatalf("Failed to load stdlib: %v", err)
    }

    // Simple test of package loading
    _, err = i.Eval(`
        package main

        import (
            "github.com/K8Trust/traefikwsbalancer"
        )

        func main() {
            config := traefikwsbalancer.CreateConfig()
            if config == nil {
                panic("config is nil")
            }
        }
    `)
    if err != nil {
        t.Fatalf("Failed to evaluate code: %v", err)
    }
}