// Copyright 2023 Emory.Du <orangeduxiaocheng@gmail.com>. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package storage

import (
	"fmt"
	uuid "github.com/satori/go.uuid"
	"testing"
	"time"
)

func TestUUIDNew(t *testing.T) {
	fmt.Println(uuid.Must(uuid.NewV4()).String())
}

func TestTimeTicker(t *testing.T) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	done := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Second)
		done <- struct{}{}
	}()

	for {
		select {
		case <-done:
			fmt.Println("Done!")
			return
		case t := <-ticker.C:
			fmt.Println("Current time:", t)
		}
	}
}

func TestTimeAfterFunc(t *testing.T) {
	ticker := time.AfterFunc(time.Second, func() { fmt.Println("hello") })
	defer ticker.Stop()

	done := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Second)
		done <- struct{}{}
	}()

	for {
		select {
		case <-done:
			fmt.Println("Done!")
			return
		}
	}
}
