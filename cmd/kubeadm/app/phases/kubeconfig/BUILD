package(default_visibility = ["//visibility:public"])

licenses(["notice"])

load(
    "@io_bazel_rules_go//go:def.bzl",
    "go_library",
    "go_test",
)

go_test(
    name = "go_default_test",
    srcs = ["kubeconfig_test.go"],
    library = ":go_default_library",
    tags = ["automanaged"],
    deps = ["//cmd/kubeadm/app/apis/kubeadm:go_default_library"],
)

go_library(
    name = "go_default_library",
    srcs = [
        "doc.go",
        "kubeconfig.go",
    ],
    tags = ["automanaged"],
    deps = [
        "//cmd/kubeadm/app/phases/certs:go_default_library",
        "//pkg/client/unversioned/clientcmd:go_default_library",
        "//vendor:k8s.io/client-go/pkg/util/cert",
        "//vendor:k8s.io/client-go/tools/clientcmd/api",
    ],
)

filegroup(
    name = "package-srcs",
    srcs = glob(["**"]),
    tags = ["automanaged"],
    visibility = ["//visibility:private"],
)

filegroup(
    name = "all-srcs",
    srcs = [":package-srcs"],
    tags = ["automanaged"],
)
