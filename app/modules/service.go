package modules

// StartCentral is set by the workers package at init time to avoid an import
// cycle (workers imports modules, but the CLI in modules starts the central).
var StartCentral func() error
