package worker

import "time"

// timeNow is the worker package's clock. Tests override it.
var timeNow = time.Now
