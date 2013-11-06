# sitkin

sitkin is yet another static blog engine written in Go.

## TODO

* MVP: an outer template that render the main index page and individual post pages (from markdown documents).
* TOML configuration
* syntax highlighting for code
* Multiple post types (specified in configuration)
* TOML front matter

## Project layout for sitkin

```
+-PROJECT_ROOT
  +-_config.toml                      # The configuration.
  +-_compiled/                        # _compiled contains the compiled site.
  +-_templates/                       # templates contains Go templates (.tmpl files).
  +-_posts/                           # posts is a directory specified in POST_DIRS in config.toml.
  | +-2013-07-24-18-19-hello-world.md # A post must be a markdown file named with a timestamp.
  +-assets/                           # Any file or directory not starting with _ is copied directly.
  | +-css/
  | +-js/
  | +-images/
  +-index.tmpl                        # Any tmpl or md file outside the above directories that contains
  +-about.md                          # TOML front matter is compiled to an html file.
```

## Config file

`_config.toml` is a TOML file.

``` toml
[category.posts]  # Any category needs a named section called category.NAME. The posts for the category will
                  # be in _NAME and the compiled html files will be in _compiled/NAME/.

template = "post" # The default template for posts in this category.
```

## Variables

TODO
