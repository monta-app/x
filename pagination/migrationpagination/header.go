// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package migrationpagination

// swagger:model mixedPaginationRequestParameters
type RequestParameters struct {
	// Items per Page
	//
	// This is the number of items per page.
	//
	// required: false
	// in: query
	// default: 250
	// min: 1
	// max: 1000
	PerPage int `json:"per_page"`

	// Pagination Page
	//
	// This value is currently an integer, but it is not sequential. The value is not the page number, but a
	// reference. The next page can be any number and some numbers might return an empty list.
	//
	// For example, page 2 might not follow after page 1. And even if page 3 and 5 exist, but page 4 might not exist.
	//
	// required: false
	// in: query
	// default: 1
	// min: 1
	Page int `json:"page"`
}

// Pagination Response Header
//
// The `Link` HTTP header contains multiple links (`first`, `next`, `last`, `previous`) formatted as:
// `<https://{project-slug}.projects.oryapis.com/admin/clients?page_size={limit}&page_token={offset}>; rel="{page}"`
//
// For details on pagination please head over to the [pagination documentation](https://www.ory.sh/docs/ecosystem/api-design#pagination).
//
// swagger:model mixedPagePaginationResponseHeaders
type ResponseHeaderAnnotation struct {
	// The Link HTTP Header
	//
	// The `Link` header contains a comma-delimited list of links to the following pages:
	//
	// - first: The first page of results.
	// - next: The next page of results.
	// - prev: The previous page of results.
	// - last: The last page of results.
	//
	// Pages are omitted if they do not exist. For example, if there is no next page, the `next` link is omitted.
	//
	// The header value may look like follows:
	//
	//	</clients?limit=5&offset=0>; rel="first",</clients?limit=5&offset=15>; rel="next",</clients?limit=5&offset=5>; rel="prev",</clients?limit=5&offset=20>; rel="last"
	Link string `json:"link"`

	// The X-Total-Count HTTP Header
	//
	// The `X-Total-Count` header contains the total number of items in the collection.
	TotalCount int `json:"x-total-count"`
}
