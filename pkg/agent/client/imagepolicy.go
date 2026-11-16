package client

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReasonImageNotPlatform is the ErrorInfo reason (domain ErrorDomain, codes.PermissionDenied) of a lab refused because
// a device image is not an image of the platform registry pinned by digest (platform:<name>@sha256:<hex>) while the
// tenant or the platform runs platform images only. The message names the variant, the device and the image.
const ReasonImageNotPlatform = "IMAGE_NOT_PLATFORM"

// ReasonRegistryNotAllowed is the ErrorInfo reason (domain ErrorDomain, codes.PermissionDenied) of a lab refused
// because a device image comes from a registry that is not on the laboratory's list of allowed registries (chart
// registry.allowedRegistries; GetFeatures reports the list). The message names the variant, the device, the image
// and its registry.
const ReasonRegistryNotAllowed = "REGISTRY_NOT_ALLOWED"

// IsRegistryNotAllowed reports whether err is the refusal of an image from a registry that is not allowed.
func IsRegistryNotAllowed(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		return false
	}
	return hasReason(st, ReasonRegistryNotAllowed)
}

// ReasonCredentialNotSupported is the ErrorInfo reason (domain ErrorDomain, codes.InvalidArgument) of a request refused because a
// registry credential has no username while the laboratory's image cache is on: the cache carries the tenant in the username
// ("<tenant>~<user>"), so a credential made of a token alone cannot reach the registry through it. The message names the registry.
const ReasonCredentialNotSupported = "CREDENTIAL_NOT_SUPPORTED"

// IsCredentialNotSupported reports whether err is the refusal of a registry credential that has no username.
func IsCredentialNotSupported(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.InvalidArgument {
		return false
	}
	return hasReason(st, ReasonCredentialNotSupported)
}

func hasReason(st *status.Status, reason string) bool {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.Domain == ErrorDomain && info.Reason == reason {
			return true
		}
	}
	return false
}

// IsImageNotPlatform reports whether err is the refusal of an image that is not a platform image.
func IsImageNotPlatform(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		return false
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.Domain == ErrorDomain && info.Reason == ReasonImageNotPlatform {
			return true
		}
	}
	return false
}
