// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

//go:build tools

// Package tools pins module dependencies used by packages that are developed
// in parallel, so `go mod tidy` keeps them before all importers exist.
package tools

import (
	_ "github.com/aws/aws-sdk-go-v2/config"
	_ "github.com/aws/aws-sdk-go-v2/credentials"
	_ "github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/golang-jwt/jwt/v5"
	_ "github.com/google/addlicense"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "k8s.io/api/core/v1"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/client-go/kubernetes"
)
