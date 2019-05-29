## [2.0] - 2017-05-10
### Added
- Support PyQt5 if available.
- Sets title from original one.
- Returns server errors as is.
- Download non-HTML files as is.
- For JavaScript capable browsers detect and automatically set view width.
- Add support for configuring which image format to use.
- Added support for PythonMagick. If found, allows to dither, color-reduce, or convert to grayscale or monochrome.
- If PythonMagick is found, render as PNG and convert to user-requested format using it, for better quality.

### Changed
- Support www prepented to http://wrp.stop command.

### Fixed
- Prevent python crashes with non-ASCII character in URLs.

## [1.4] - 2017-01-22
### Added
- Suport for ISMAP on Linux.
- Use queues instead of globals in Linux.

## [1.3] - 2017-01-21
### Changed
- Merged mac OS and Linux in a single executable.
- Use queues instead of globals in Linux.

### Fixed
- Call PyQt to close application on http://wrp.stop

## [1.2] - 2016-12-27
### Added
- Support for IMAP on mac OS.

### Changed
- Use queues instead of globals in mac OS.