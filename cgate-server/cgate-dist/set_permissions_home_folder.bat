@echo off
rem **********
rem ** Set Permissions (must be run as Administrator)
rem **********
pushd %~dp0

set working_dir=%1

rem For root directory with recursion: block inherited permissions, clear existing permissions, Auth Users group can modify, Network Service account can modify, Administrators group has full permissions
setacl -on %working_dir% -ot file -rec cont_obj -actn setprot -op "dacl:p_nc;sacl:p_nc" -actn clear -clr "dacl,sacl" -actn ace -ace "n:S-1-5-11;p:change"  -ace "n:S-1-5-20;p:change"  -ace "n:S-1-5-32-544;p:full" 

rem For Projects directory: allow inherited permissions so we can copy old project files into this directory
setacl -on %working_dir% -ot file -actn setprot -op "dacl:np;sacl:np" -actn ace -ace "n:S-1-5-11;p:change"  -ace "n:S-1-5-20;p:change"  -ace "n:S-1-5-32-544;p:full" 

popd
