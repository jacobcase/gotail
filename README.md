Simple no-nonsense file tailing.

## Installation

`go get -u github.com/jacobcase/gotail`

## Overview

gotail is a simple go module that provides regular file tailing.
While there are a few other tail libraries, many of them
are some combination of unmaintained, buggy, or overly complex.

gotail currently exposes a simple file polling implementation. It does not assume
that you only want to read newline delimited text data, nor does it assume you
want to consume it over a channel. Instead, implementations in this module expose
file tailing as a simple io.ReadCloser that never reaches EOF, transparently
consuming files as they are rotated.

Polling may be excessive for some applications. This module was designed with
large and frequently written log files in mind, such as edge proxy logs.

## Platforms

So far, gotail has only been testing on Linux. However, the poller implementation 
doesn't depend on any OS specific features that aren't abstracted by the OS package,
so it should work on most systems.

## Contributing
Contributions welcome! An fsnotify implementation would be nice and I may get around
to adding it some day.

## TODO
* File checkpoints for resuming on restart
* Readline implementation that can provide line aligned checkpoints.
* More thorough testing 
    * Test seeking
    * Test correct offset tracking