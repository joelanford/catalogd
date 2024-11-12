package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gorilla/handlers"
	"github.com/itchyny/gojq"
	"github.com/spf13/cobra"

	"github.com/operator-framework/catalogd/cmd/server/internal/cache"
)

func main() {
	all := cobra.Command{
		Use:  "all",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			catalogdServer := getCatalogdServer(args[0])
			log.Printf("Starting all server on %s", catalogdServer.Addr)
			if err := runServer(cmd.Context(), catalogdServer, 5*time.Second); err != nil {
				log.Fatal(err)
			}
		},
	}
	query := cobra.Command{
		Use:  "query <cacheDir> <addr>",
		Args: cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			queryCache := cache.NewCache(args[0], 30*time.Minute)
			queryServer := getQueryServer(queryCache, args[1])
			log.Printf("Starting query server on %s", queryServer.Addr)
			if err := runServer(cmd.Context(), queryServer, 5*time.Second); err != nil {
				log.Fatal(err)
			}
		},
	}
	root := cobra.Command{
		Use: "catalogd",
	}
	root.AddCommand(&all, &query)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		log.Fatal(err)
	}
}

func runServer(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration) error {
	// Start server in a separate goroutine.
	errChan := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- fmt.Errorf("server error: %w", err)
		}
		close(errChan)
	}()

	// Listen for context cancellation to trigger shutdown.
	select {
	case <-ctx.Done():
		ctxShutdown, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctxShutdown); err != nil {
			return fmt.Errorf("shutdown error: %w", err)
		}
		return nil
	case err := <-errChan:
		return err // Return error from ListenAndServe if any.
	}
}

func getCatalogdServer(baseDir string) *http.Server {
	h := http.NewServeMux()
	h.Handle("GET /catalogs/{catalogName}/api/v1/all", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fileName := filepath.Join(baseDir, r.PathValue("catalogName"), "catalog.json")
		fmt.Println(fileName)
		http.ServeFile(w, r, filepath.Join(baseDir, r.PathValue("catalogName"), "catalog.json"))
	}))
	s := http.Server{
		Addr:    ":8080",
		Handler: handlers.LoggingHandler(os.Stdout, h),
	}
	return &s
}

func getQueryServer(c *cache.Cache, addr string) *http.Server {
	h := http.NewServeMux()
	upstreamPattern := "http://%s/catalogs/%s/api/v1/all"
	h.Handle("GET /catalogs/{catalogName}/api/v1/query", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogName := r.PathValue("catalogName")
		schema := r.URL.Query().Get("schema")
		packageName := r.URL.Query().Get("package")
		name := r.URL.Query().Get("name")

		//if schema == "" && packageName == "" && name == "" {
		//	http.Error(w, "at least one query parameter in [schema,package,name] is required", http.StatusBadRequest)
		//	return
		//}

		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf(upstreamPattern, addr, catalogName), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		existingCatalogFile, existingCatalogIndex, existingCatalogModTime, ok := c.Get(catalogName)
		if ok {
			defer func() {
				if err := existingCatalogFile.Close(); err != nil {
					log.Printf("error closing catalog file: %v", err)
				}
			}()
			req.Header.Set("If-Modified-Since", existingCatalogModTime.Format(http.TimeFormat))
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		var (
			catalogFile    *os.File
			catalogIndex   *cache.Index
			catalogModTime *time.Time
		)
		switch resp.StatusCode {
		case http.StatusOK:
			newModTime, err := time.Parse(http.TimeFormat, resp.Header.Get("Last-Modified"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			newFile, newIndex, err := c.Set(catalogName, resp.Body, &newModTime)
			if err != nil {
				http.Error(w, fmt.Errorf("could not store response in cache: %v", err).Error(), http.StatusInternalServerError)
				return
			}
			defer func() {
				if err := newFile.Close(); err != nil {
					log.Printf("error closing catalog file: %v", err)
				}
			}()
			catalogFile = newFile
			catalogIndex = newIndex
			catalogModTime = &newModTime
		case http.StatusNotModified:
			if r.Header.Get("If-Modified-Since") == resp.Header.Get("Last-Modified") {
				w.WriteHeader(http.StatusNotModified)
				return
			}

			catalogFile = existingCatalogFile
			catalogIndex = existingCatalogIndex
			catalogModTime = existingCatalogModTime
		default:
			http.Error(w, resp.Status, resp.StatusCode)
			return
		}

		qReader, ok := catalogIndex.Get(catalogFile, schema, packageName, name)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/jsonl")
		w.Header().Set("Last-Modified", catalogModTime.Format(http.TimeFormat))

		jqQuery := r.URL.Query().Get("jq")
		if jqQuery == "" {
			_, _ = io.Copy(w, qReader)
			return
		}

		query, err := gojq.Parse(jqQuery)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		code, err := gojq.Compile(query)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		dec := json.NewDecoder(qReader)
		enc := json.NewEncoder(w)
		for {
			var blob map[string]any
			if err := dec.Decode(&blob); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			iter := code.Run(blob)
			for {
				v, ok := iter.Next()
				if !ok {
					break
				}
				if _, ok := v.(error); ok {
					var err *gojq.HaltError
					if errors.As(err, &err) && err.Value() == nil {
						break
					}
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if err := enc.Encode(v); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
		}
	}))
	s := http.Server{
		Addr:    ":8081",
		Handler: handlers.LoggingHandler(os.Stdout, h),
	}
	return &s
}
