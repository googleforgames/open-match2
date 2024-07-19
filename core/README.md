open skaffold.yaml, jump to the profiles section

Under the gcp profile, replace all references to region and project id with yours, save & exit

`skaffold diagnose -p gcp` to review the changes

`skaffold dev -p gcp --port-forward` to run the skaffold dev loop
